package httpserver_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/config"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
)

type backupExporterFunc func(context.Context, http.ResponseWriter, []byte) (string, error)

func (f backupExporterFunc) Export(ctx context.Context, w http.ResponseWriter, passphrase []byte) (string, error) {
	return f(ctx, w, passphrase)
}

type backupImporterFunc func(context.Context, io.Reader, int64, []byte, bool) (httpserver.ImportReport, error)

func (f backupImporterFunc) Import(ctx context.Context, body io.Reader, size int64, passphrase []byte, dryRun bool) (httpserver.ImportReport, error) {
	return f(ctx, body, size, passphrase, dryRun)
}

type backupHTTPFixture struct {
	server         *httpserver.Server
	ownerCookie    *http.Cookie
	operatorCookie *http.Cookie
}

func newBackupHTTPFixture(t *testing.T, exporter httpserver.BackupExporter, importer httpserver.BackupImporter, logger *log.Logger) *backupHTTPFixture {
	t.Helper()
	store := newBackupHTTPAuthStore()
	authService := auth.NewService(store, auth.ServiceOptions{SessionTTL: time.Hour})
	owner, err := authService.Bootstrap(context.Background(), "owner", "owner-password")
	if err != nil {
		t.Fatalf("bootstrap owner: %v", err)
	}
	operator, err := authService.CreateUser(context.Background(), owner, "operator", "operator-password", auth.RoleOperator)
	if err != nil {
		t.Fatalf("create operator: %v", err)
	}
	ownerSession, err := authService.Login(context.Background(), owner.Username, "owner-password")
	if err != nil {
		t.Fatalf("login owner: %v", err)
	}
	operatorSession, err := authService.Login(context.Background(), operator.Username, "operator-password")
	if err != nil {
		t.Fatalf("login operator: %v", err)
	}
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	server := httpserver.New(config.Config{
		UIDist:            t.TempDir(),
		SessionCookieName: "proxycore_session",
		SessionTTL:        time.Hour,
	}, logger, httpserver.WithAuthService(authService), httpserver.WithBackup(exporter, importer))
	return &backupHTTPFixture{
		server:         server,
		ownerCookie:    &http.Cookie{Name: "proxycore_session", Value: ownerSession.Token},
		operatorCookie: &http.Cookie{Name: "proxycore_session", Value: operatorSession.Token},
	}
}

func TestBackupHandlersRequireOwner(t *testing.T) {
	exporter := backupExporterFunc(func(_ context.Context, w http.ResponseWriter, _ []byte) (string, error) {
		_, _ = w.Write([]byte("zip"))
		return "abc123", nil
	})
	importer := backupImporterFunc(func(_ context.Context, _ io.Reader, _ int64, _ []byte, _ bool) (httpserver.ImportReport, error) {
		return httpserver.ImportReport{}, nil
	})

	tests := []struct {
		name   string
		target string
		cookie *http.Cookie
		want   int
	}{
		{name: "anonymous export", target: "/api/backup/export", want: http.StatusUnauthorized},
		{name: "operator export", target: "/api/backup/export", want: http.StatusForbidden},
		{name: "anonymous import", target: "/api/backup/import?dry_run=true", want: http.StatusUnauthorized},
		{name: "operator import", target: "/api/backup/import?dry_run=true", want: http.StatusForbidden},
	}
	fixture := newBackupHTTPFixture(t, exporter, importer, nil)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cookie := tt.cookie
			if strings.Contains(tt.name, "operator") {
				cookie = fixture.operatorCookie
			}
			req := httptest.NewRequest(http.MethodPost, tt.target, nil)
			if cookie != nil {
				req.AddCookie(cookie)
			}
			rec := httptest.NewRecorder()
			fixture.server.Handler().ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("status=%d body=%s, want %d", rec.Code, rec.Body.String(), tt.want)
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			wantError := "Authentication required"
			if tt.want == http.StatusForbidden {
				wantError = "Permission denied"
			}
			if body["error"] != wantError {
				t.Fatalf("error=%q, want %q", body["error"], wantError)
			}
		})
	}
}

func TestBackupExportHappyPath(t *testing.T) {
	const manifestSHA = "A1B2C3"
	var gotPassphrase []byte
	fixture := newBackupHTTPFixture(t, backupExporterFunc(func(_ context.Context, w http.ResponseWriter, passphrase []byte) (string, error) {
		gotPassphrase = append([]byte(nil), passphrase...)
		_, _ = w.Write([]byte("known zip bytes"))
		return manifestSHA, nil
	}), nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/backup/export", nil)
	req.AddCookie(fixture.ownerCookie)
	req.Header.Set("X-Proxycore-Backup-Passphrase", base64.RawURLEncoding.EncodeToString([]byte("secret")))
	rec := httptest.NewRecorder()
	fixture.server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "known zip bytes" {
		t.Fatalf("body=%q", got)
	}
	result := rec.Result()
	if got := result.Header.Get("Content-Type"); got != "application/zip" {
		t.Fatalf("content type=%q", got)
	}
	contentDisposition := result.Header.Get("Content-Disposition")
	if !strings.HasPrefix(contentDisposition, `attachment; filename="proxycore-backup-`) || !strings.HasSuffix(contentDisposition, `.zip"`) {
		t.Fatalf("content disposition=%q", contentDisposition)
	}
	if got := result.Trailer.Get("Audit-Version"); got != strings.ToLower(manifestSHA) {
		t.Fatalf("audit version trailer=%q", got)
	}
	if string(gotPassphrase) != "secret" {
		t.Fatalf("passphrase=%q", gotPassphrase)
	}
}

func TestBackupExportAuditVersionIsDeclaredAsTrailer(t *testing.T) {
	const manifestSHA = "D4E5F6"
	fixture := newBackupHTTPFixture(t, backupExporterFunc(func(_ context.Context, w http.ResponseWriter, _ []byte) (string, error) {
		_, _ = w.Write([]byte("known zip bytes"))
		return manifestSHA, nil
	}), nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/backup/export", nil)
	req.AddCookie(fixture.ownerCookie)
	rec := httptest.NewRecorder()
	fixture.server.Handler().ServeHTTP(rec, req)

	result := rec.Result()
	if got := result.Header.Get("Audit-Version"); got != "" {
		t.Fatalf("audit version was sent as a leading header: %q", got)
	}
	if got := result.Header.Get("Trailer"); got != "Audit-Version" {
		t.Fatalf("trailer declaration=%q", got)
	}
}

func TestBackupExportMissingService(t *testing.T) {
	fixture := newBackupHTTPFixture(t, nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/backup/export", nil)
	req.AddCookie(fixture.ownerCookie)
	rec := httptest.NewRecorder()
	fixture.server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	assertBackupError(t, rec, "backup service is not configured")
}

func TestBackupExportFailureBeforeHeaders(t *testing.T) {
	fixture := newBackupHTTPFixture(t, backupExporterFunc(func(context.Context, http.ResponseWriter, []byte) (string, error) {
		return "", errors.New("export failed")
	}), nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/backup/export", nil)
	req.AddCookie(fixture.ownerCookie)
	rec := httptest.NewRecorder()
	fixture.server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	assertBackupError(t, rec, "backup export failed")
	if got := rec.Header().Get("Content-Disposition"); got != "" {
		t.Fatalf("content disposition on failed response=%q", got)
	}
}

func TestBackupExportFailureAfterHeaders(t *testing.T) {
	fixture := newBackupHTTPFixture(t, backupExporterFunc(func(_ context.Context, w http.ResponseWriter, _ []byte) (string, error) {
		_, _ = w.Write([]byte("partial"))
		return "", errors.New("export failed")
	}), nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/backup/export", nil)
	req.AddCookie(fixture.ownerCookie)
	rec := httptest.NewRecorder()
	fixture.server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "partial" {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "backup export failed") {
		t.Fatal("handler appended JSON error after streaming started")
	}
}

func TestBackupExportWarnsForPassphraseOverPlainHTTPWithoutLeakingIt(t *testing.T) {
	var logs bytes.Buffer
	fixture := newBackupHTTPFixture(t, backupExporterFunc(func(_ context.Context, w http.ResponseWriter, passphrase []byte) (string, error) {
		if string(passphrase) != "top-secret" {
			t.Fatalf("passphrase=%q", passphrase)
		}
		_, _ = w.Write([]byte("zip"))
		return "hash", nil
	}), nil, log.New(&logs, "", 0))
	req := httptest.NewRequest(http.MethodPost, "/api/backup/export", nil)
	req.AddCookie(fixture.ownerCookie)
	req.Header.Set("X-Proxycore-Backup-Passphrase", base64.RawURLEncoding.EncodeToString([]byte("top-secret")))
	rec := httptest.NewRecorder()
	fixture.server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(logs.String(), "top-secret") {
		t.Fatalf("passphrase leaked in logs: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "INFO") {
		t.Fatalf("missing plaintext passphrase warning: %s", logs.String())
	}
}

func TestBackupImportHappyDryRunUsesQueryAsSourceOfTruth(t *testing.T) {
	var gotDryRun bool
	var gotBundle []byte
	fixture := newBackupHTTPFixture(t, nil, backupImporterFunc(func(_ context.Context, body io.Reader, size int64, _ []byte, dryRun bool) (httpserver.ImportReport, error) {
		gotDryRun = dryRun
		if size != int64(len("bundle")) {
			t.Fatalf("size=%d", size)
		}
		var err error
		gotBundle, err = io.ReadAll(body)
		if err != nil {
			t.Fatalf("read bundle: %v", err)
		}
		return httpserver.ImportReport{DryRun: dryRun, Tables: []httpserver.TablePreview{{Name: "users", RowCount: 2, Action: "preview"}}}, nil
	}), nil)
	req := newBackupImportRequest(t, "/api/backup/import?dry_run=true", fixture.ownerCookie, []byte("bundle"), "secret", "false")
	rec := httptest.NewRecorder()
	fixture.server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var report httpserver.ImportReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if !report.DryRun || !gotDryRun || string(gotBundle) != "bundle" {
		t.Fatalf("report=%+v dryRun=%v bundle=%q", report, gotDryRun, gotBundle)
	}
	if len(report.Tables) != 1 || report.Tables[0].Name != "users" || report.Tables[0].RowCount != 2 || report.Tables[0].Action != "preview" {
		t.Fatalf("tables=%+v", report.Tables)
	}
}

func TestBackupImportReal(t *testing.T) {
	var gotDryRun bool
	fixture := newBackupHTTPFixture(t, nil, backupImporterFunc(func(_ context.Context, _ io.Reader, _ int64, _ []byte, dryRun bool) (httpserver.ImportReport, error) {
		gotDryRun = dryRun
		return httpserver.ImportReport{DryRun: dryRun, AppliedPostImport: true}, nil
	}), nil)
	req := newBackupImportRequest(t, "/api/backup/import?dry_run=false", fixture.ownerCookie, []byte("bundle"), "", "true")
	rec := httptest.NewRecorder()
	fixture.server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || gotDryRun {
		t.Fatalf("status=%d dryRun=%v body=%s", rec.Code, gotDryRun, rec.Body.String())
	}
	var report httpserver.ImportReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if report.DryRun || !report.AppliedPostImport {
		t.Fatalf("report=%+v", report)
	}
}

func TestBackupImportMissingService(t *testing.T) {
	fixture := newBackupHTTPFixture(t, nil, nil, nil)
	req := newBackupImportRequest(t, "/api/backup/import?dry_run=true", fixture.ownerCookie, []byte("bundle"), "", "")
	rec := httptest.NewRecorder()
	fixture.server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	assertBackupError(t, rec, "backup service is not configured")
}

func TestBackupImportErrors(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		wantError  string
	}{
		{name: "wrong passphrase", err: httpserver.ErrPassphraseMismatch, wantStatus: http.StatusUnauthorized, wantError: "passphrase mismatch"},
		{name: "master key mismatch", err: httpserver.ErrMasterKeyMismatch, wantStatus: http.StatusConflict, wantCode: "master_key_mismatch", wantError: "master key mismatch"},
		{name: "corrupt archive", err: httpserver.ErrCorruptArchive, wantStatus: http.StatusBadRequest, wantError: "invalid backup archive"},
		{name: "path escape", err: httpserver.ErrPathEscape, wantStatus: http.StatusBadRequest, wantError: "invalid backup archive"},
		{name: "unsupported version", err: httpserver.ErrUnsupportedVersion, wantStatus: http.StatusBadRequest, wantError: "unsupported backup version"},
		{name: "passphrase required", err: httpserver.ErrPassphraseRequired, wantStatus: http.StatusBadRequest, wantError: "passphrase required"},
		{name: "body too large from importer", err: &http.MaxBytesError{Limit: 4 << 30}, wantStatus: http.StatusRequestEntityTooLarge, wantError: "backup bundle is too large"},
		{name: "validation", err: errors.New("invalid manifest validation"), wantStatus: http.StatusBadRequest, wantError: "invalid backup import"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newBackupHTTPFixture(t, nil, backupImporterFunc(func(context.Context, io.Reader, int64, []byte, bool) (httpserver.ImportReport, error) {
				return httpserver.ImportReport{}, tt.err
			}), nil)
			req := newBackupImportRequest(t, "/api/backup/import?dry_run=true", fixture.ownerCookie, []byte("bundle"), "", "")
			rec := httptest.NewRecorder()
			fixture.server.Handler().ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status=%d body=%s, want %d", rec.Code, rec.Body.String(), tt.wantStatus)
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if body["error"] != tt.wantError || body["code"] != tt.wantCode {
				t.Fatalf("body=%v, want error=%q code=%q", body, tt.wantError, tt.wantCode)
			}
		})
	}
}

func TestBackupImportNeverEchoesPassphrase(t *testing.T) {
	const secret = "top-secret"
	fixture := newBackupHTTPFixture(t, nil, backupImporterFunc(func(context.Context, io.Reader, int64, []byte, bool) (httpserver.ImportReport, error) {
		return httpserver.ImportReport{}, errors.New("validation failed for " + secret)
	}), nil)
	req := newBackupImportRequest(t, "/api/backup/import?dry_run=true", fixture.ownerCookie, []byte("bundle"), secret, "")
	rec := httptest.NewRecorder()
	fixture.server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestBackupImportMalformedPassphrase(t *testing.T) {
	fixture := newBackupHTTPFixture(t, nil, backupImporterFunc(func(context.Context, io.Reader, int64, []byte, bool) (httpserver.ImportReport, error) {
		t.Fatal("importer called for malformed passphrase")
		return httpserver.ImportReport{}, nil
	}), nil)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("bundle", "backup.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("bundle")); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("passphrase", "not base64url!!!"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/backup/import?dry_run=true", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.AddCookie(fixture.ownerCookie)
	rec := httptest.NewRecorder()
	fixture.server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "not base64url") {
		t.Fatalf("encoded passphrase leaked: %s", rec.Body.String())
	}
}

func TestBackupImportMissingBundle(t *testing.T) {
	fixture := newBackupHTTPFixture(t, nil, backupImporterFunc(func(context.Context, io.Reader, int64, []byte, bool) (httpserver.ImportReport, error) {
		t.Fatal("importer called without bundle")
		return httpserver.ImportReport{}, nil
	}), nil)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("passphrase", ""); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/backup/import?dry_run=true", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.AddCookie(fixture.ownerCookie)
	rec := httptest.NewRecorder()
	fixture.server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestBackupImportBodyTooLarge(t *testing.T) {
	fixture := newBackupHTTPFixture(t, nil, backupImporterFunc(func(context.Context, io.Reader, int64, []byte, bool) (httpserver.ImportReport, error) {
		t.Fatal("importer called after body-size failure")
		return httpserver.ImportReport{}, nil
	}), nil)
	req := httptest.NewRequest(http.MethodPost, "/api/backup/import?dry_run=true", backupMaxBytesErrorReader{})
	req.Header.Set("Content-Type", `multipart/form-data; boundary=backup-boundary`)
	req.AddCookie(fixture.ownerCookie)
	rec := httptest.NewRecorder()
	fixture.server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

type backupMaxBytesErrorReader struct{}

func (backupMaxBytesErrorReader) Read([]byte) (int, error) {
	return 0, &http.MaxBytesError{Limit: 4 << 30}
}

func newBackupImportRequest(t *testing.T, target string, cookie *http.Cookie, bundle []byte, passphrase, formDryRun string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("bundle", "backup.zip")
	if err != nil {
		t.Fatalf("create bundle part: %v", err)
	}
	if _, err := part.Write(bundle); err != nil {
		t.Fatalf("write bundle part: %v", err)
	}
	if passphrase != "" {
		if err := writer.WriteField("passphrase", base64.RawURLEncoding.EncodeToString([]byte(passphrase))); err != nil {
			t.Fatalf("write passphrase: %v", err)
		}
	}
	if formDryRun != "" {
		if err := writer.WriteField("dry_run", formDryRun); err != nil {
			t.Fatalf("write dry_run: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, target, &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	if cookie != nil {
		req.AddCookie(cookie)
	}
	return req
}

func assertBackupError(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if body["error"] != want {
		t.Fatalf("error=%q, want %q", body["error"], want)
	}
}

type backupHTTPAuthStore struct {
	users    map[string]auth.User
	sessions map[string]auth.Session
}

func newBackupHTTPAuthStore() *backupHTTPAuthStore {
	return &backupHTTPAuthStore{users: map[string]auth.User{}, sessions: map[string]auth.Session{}}
}

func (s *backupHTTPAuthStore) ListUsers(context.Context) ([]auth.User, error) {
	users := make([]auth.User, 0, len(s.users))
	for _, user := range s.users {
		users = append(users, user)
	}
	return users, nil
}

func (s *backupHTTPAuthStore) FindUserByUsername(_ context.Context, username string) (*auth.User, error) {
	for _, user := range s.users {
		if user.Username == username {
			copy := user
			return &copy, nil
		}
	}
	return nil, nil
}

func (s *backupHTTPAuthStore) FindUserByID(_ context.Context, id string) (*auth.User, error) {
	user, ok := s.users[id]
	if !ok {
		return nil, nil
	}
	copy := user
	return &copy, nil
}

func (s *backupHTTPAuthStore) CreateUser(_ context.Context, user auth.User) (auth.User, error) {
	s.users[user.ID] = user
	return user, nil
}

func (s *backupHTTPAuthStore) UpdateUser(_ context.Context, id string, patch auth.UserPatch) (auth.User, error) {
	user, ok := s.users[id]
	if !ok {
		return auth.User{}, errors.New("user not found")
	}
	if patch.Role != nil {
		user.Role = *patch.Role
	}
	if patch.Active != nil {
		user.Active = *patch.Active
	}
	if patch.PasswordHash != nil {
		user.PasswordHash = *patch.PasswordHash
	}
	s.users[id] = user
	return user, nil
}

func (s *backupHTTPAuthStore) DeleteUser(_ context.Context, id string) error {
	delete(s.users, id)
	return nil
}

func (s *backupHTTPAuthStore) CreateSession(_ context.Context, session auth.Session) (auth.Session, error) {
	s.sessions[session.TokenHash] = session
	return session, nil
}

func (s *backupHTTPAuthStore) FindSessionByTokenHash(_ context.Context, hash string) (*auth.Session, error) {
	session, ok := s.sessions[hash]
	if !ok {
		return nil, nil
	}
	copy := session
	return &copy, nil
}

func (s *backupHTTPAuthStore) RevokeSession(_ context.Context, id string, at time.Time) error {
	for hash, session := range s.sessions {
		if session.ID == id {
			session.RevokedAt = &at
			s.sessions[hash] = session
		}
	}
	return nil
}

func (s *backupHTTPAuthStore) TouchSession(_ context.Context, id string, at time.Time) error {
	for hash, session := range s.sessions {
		if session.ID == id {
			session.LastSeenAt = &at
			s.sessions[hash] = session
		}
	}
	return nil
}

func (s *backupHTTPAuthStore) AddAudit(context.Context, auth.AuditEvent) error { return nil }

func (s *backupHTTPAuthStore) ResetPassword(context.Context, string, string, time.Time, auth.AuditEvent) (auth.User, error) {
	return auth.User{}, errors.New("not implemented")
}

func (s *backupHTTPAuthStore) CompletePasswordChange(context.Context, string, string, string, time.Time, auth.AuditEvent) (auth.User, error) {
	return auth.User{}, errors.New("not implemented")
}
