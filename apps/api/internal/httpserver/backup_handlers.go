package httpserver

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
)

// BackupExporter is the HTTP boundary for a streaming backup export. It is
// intentionally opaque for compatibility with the original response-writer
// adapter; the handler accepts both that shape and the io.Writer adapter used
// by production backupcore composition.
type BackupExporter interface{}

type streamingBackupExporter interface {
	Export(ctx context.Context, w io.Writer, passphrase []byte) (manifestSHA256Hex string, err error)
}

type responseBackupExporter interface {
	Export(ctx context.Context, w http.ResponseWriter, passphrase []byte) (manifestSHA256Hex string, err error)
}

// BackupImporter is the HTTP boundary for a backup import.
type BackupImporter interface {
	Import(ctx context.Context, body io.Reader, size int64, passphrase []byte, dryRun bool) (ImportReport, error)
}

type actorBackupImporter interface {
	ImportWithActorID(ctx context.Context, body io.Reader, size int64, passphrase []byte, dryRun bool, actorID string) (ImportReport, error)
}

// ImportReport describes the changes an import would make or made.
type ImportReport struct {
	DryRun                 bool           `json:"dryRun"`
	BundleSHA256           string         `json:"bundleSha256"`
	FormatVersion          string         `json:"formatVersion"`
	ExporterVersion        string         `json:"exporterVersion"`
	CreatedAt              string         `json:"createdAt"`
	InstallationID         string         `json:"installationId"`
	NodeID                 string         `json:"nodeId"`
	Tables                 []TablePreview `json:"tables"`
	EnvWouldChange         bool           `json:"envWouldChange"`
	CertsToRestore         []string       `json:"certsToRestore"`
	AppliedPostImport      bool           `json:"appliedPostImport"`
	CompatibilityConflicts []string       `json:"compatibilityConflicts,omitempty"`
}

// TablePreview is the import action for one configuration table.
type TablePreview struct {
	Name     string `json:"name"`
	RowCount int    `json:"rowCount"`
	Action   string `json:"action"`
}

// These errors form the import boundary's stable error classification. The
// archive package uses the same messages, so wrapped errors from that package
// remain classifiable without coupling this HTTP package to its implementation.
var (
	ErrPassphraseMismatch = errors.New("zipwriter: passphrase mismatch")
	ErrCorruptArchive     = errors.New("zipwriter: corrupt archive")
	ErrPathEscape         = errors.New("zipwriter: path escapes archive root")
	ErrUnsupportedVersion = errors.New("zipwriter: unsupported version")
	ErrPassphraseRequired = errors.New("zipwriter: passphrase required")
	ErrMasterKeyMismatch  = errors.New("master key mismatch")
)

const backupMaxBodySize int64 = 4 << 30

func (s *Server) handleBackupExport(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r, auth.RoleOwner); !ok {
		return
	}
	if s.backupExporter == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "backup service is not configured"})
		return
	}

	encodedPassphrase := r.Header.Get("X-Proxycore-Backup-Passphrase")
	if encodedPassphrase != "" && r.TLS == nil && s.log != nil {
		s.log.Printf("INFO backup passphrase received over non-TLS request")
	}
	passphrase, err := decodeBackupPassphrase(encodedPassphrase)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid backup passphrase"})
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	// Use a colon-free timestamp so the generated backup filename is safe on Windows.
	w.Header().Set("Content-Disposition", `attachment; filename="proxycore-backup-`+time.Now().UTC().Format("2006-01-02T15-04-05Z")+`.zip"`)
	w.Header().Add("Trailer", "Audit-Version")
	tracked := &backupResponseWriter{ResponseWriter: w}
	manifestSHA256Hex, exportErr := callBackupExport(s.backupExporter, r.Context(), tracked, passphrase)
	if exportErr != nil {
		if tracked.wroteHeader {
			if s.log != nil {
				s.log.Printf("WARN backup export failed after response started")
			}
			return
		}
		w.Header().Del("Content-Type")
		w.Header().Del("Content-Disposition")
		w.Header().Del("Trailer")
		w.Header().Del("Audit-Version")
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "backup export failed"})
		return
	}

	// Export returns the manifest digest after it has completed. The trailer
	// declaration above lets net/http append this value after the streamed body.
	w.Header().Set("Audit-Version", strings.ToLower(manifestSHA256Hex))
	if !tracked.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
}

func (s *Server) handleBackupImport(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r, auth.RoleOwner)
	if !ok {
		return
	}
	if s.backupImporter == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "backup service is not configured"})
		return
	}

	dryRun, err := parseBackupDryRun(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "dry_run must be true or false"})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, backupMaxBodySize)
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeBackupMultipartError(w, err)
		return
	}
	if r.MultipartForm != nil {
		defer func() { _ = r.MultipartForm.RemoveAll() }()
	}

	file, header, err := r.FormFile("bundle")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bundle file is required"})
		return
	}
	defer file.Close()
	if header.Size > backupMaxBodySize {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "backup bundle is too large"})
		return
	}

	passphrase, err := decodeBackupPassphrase(r.PostFormValue("passphrase"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid backup passphrase"})
		return
	}

	var report ImportReport
	if actorImporter, ok := s.backupImporter.(actorBackupImporter); ok {
		report, err = actorImporter.ImportWithActorID(r.Context(), file, header.Size, passphrase, dryRun, user.ID)
	} else {
		report, err = s.backupImporter.Import(r.Context(), file, header.Size, passphrase, dryRun)
	}
	if err != nil {
		writeBackupImportError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func callBackupExport(exporter BackupExporter, ctx context.Context, tracked *backupResponseWriter, passphrase []byte) (string, error) {
	switch typed := exporter.(type) {
	case streamingBackupExporter:
		return typed.Export(ctx, tracked, passphrase)
	case responseBackupExporter:
		return typed.Export(ctx, tracked, passphrase)
	default:
		return "", errors.New("backup exporter does not implement a supported export signature")
	}
}

func parseBackupDryRun(r *http.Request) (bool, error) {
	switch r.URL.Query().Get("dry_run") {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, errors.New("invalid dry_run")
	}
}

func decodeBackupPassphrase(encoded string) ([]byte, error) {
	if encoded == "" {
		return nil, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err == nil {
		return decoded, nil
	}
	return base64.URLEncoding.DecodeString(encoded)
}

func writeBackupMultipartError(w http.ResponseWriter, err error) {
	var maxBytesError *http.MaxBytesError
	if errors.As(err, &maxBytesError) || strings.Contains(strings.ToLower(err.Error()), "request body too large") {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "backup bundle is too large"})
		return
	}
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid multipart backup request"})
}

func writeBackupImportError(w http.ResponseWriter, err error) {
	if isBackupMaxBytesError(err) {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "backup bundle is too large"})
		return
	}
	switch {
	case backupErrorIs(err, ErrMasterKeyMismatch) || backupErrorContains(err, "master key mismatch"):
		writeJSON(w, http.StatusConflict, map[string]string{
			"code":  "master_key_mismatch",
			"error": "master key mismatch",
		})
	case backupErrorIs(err, ErrPassphraseMismatch) || backupErrorContains(err, "passphrase mismatch"):
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "passphrase mismatch"})
	case backupErrorIs(err, ErrCorruptArchive) || backupErrorContains(err, "corrupt archive"),
		backupErrorIs(err, ErrPathEscape) || backupErrorContains(err, "path escapes archive root"):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid backup archive"})
	case backupErrorIs(err, ErrUnsupportedVersion) || backupErrorContains(err, "unsupported version"):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported backup version"})
	case backupErrorIs(err, ErrPassphraseRequired) || backupErrorContains(err, "passphrase required"):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "passphrase required"})
	default:
		// Import validation errors are client-facing, but the original error is
		// intentionally not rendered because it could contain a passphrase.
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid backup import"})
	}
}

func isBackupMaxBytesError(err error) bool {
	var maxBytesError *http.MaxBytesError
	message := strings.ToLower(err.Error())
	return errors.As(err, &maxBytesError) ||
		strings.Contains(message, "request body too large") ||
		strings.Contains(message, "body exceeds configured limit")
}

func backupErrorIs(err, sentinel error) bool {
	if errors.Is(err, sentinel) {
		return true
	}
	for current := err; current != nil; current = errors.Unwrap(current) {
		if current.Error() == sentinel.Error() {
			return true
		}
	}
	return false
}

func backupErrorContains(err error, phrase string) bool {
	for current := err; current != nil; current = errors.Unwrap(current) {
		if strings.Contains(strings.ToLower(current.Error()), strings.ToLower(phrase)) {
			return true
		}
	}
	return false
}

type backupResponseWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *backupResponseWriter) WriteHeader(statusCode int) {
	if !w.wroteHeader {
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *backupResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

func (w *backupResponseWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *backupResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
