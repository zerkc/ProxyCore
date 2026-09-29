package zipextract

import (
	"archive/zip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"sync"
)

const (
	envelopeMagic        = "BX01"
	envelopeVersion      = byte(0x01)
	envelopeKDFID        = byte(0x01)
	kdfIterations        = 600_000
	kdfRParam            = byte(8)
	kdfPParam            = byte(1)
	envelopeReserved     = byte(0)
	envelopeSaltSize     = 16
	envelopeNonceSize    = 12
	envelopeHeaderSize   = 81
	envelopeChunkSize    = 64 * 1024
	envelopeTagSize      = 16
	bodyKeyContext       = "proxycore-backup-body-v1"
	verifierMessage      = "proxycore-backup-v1"
	maxExtractedFileSize = int64(2 << 30)
)

var errArchiveClosed = errors.New("zipwriter: archive closed")

// File is a validated archive entry. Open returns a fresh reader for the
// entry and does not expose the archive's backing storage.
type File struct {
	Path string
	Size int64
	Open func() (io.ReadCloser, error)
}

// Archive is the read-side view consumed by the import engine.
type Archive interface {
	Files(ctx context.Context) ([]File, error)
	Close() error
}

// Open detects a raw ZIP or BX01 envelope and validates its central directory
// before returning an archive handle.
func Open(ctx context.Context, in io.Reader, passphrase []byte) (Archive, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if in == nil {
		return nil, ErrCorruptArchive
	}
	if err := contextErr(ctx); err != nil {
		return nil, err
	}

	var prefix [len(envelopeMagic)]byte
	n, _ := io.ReadFull(in, prefix[:])
	if n == len(prefix) && string(prefix[:]) == envelopeMagic {
		if len(passphrase) == 0 {
			return nil, ErrPassphraseRequired
		}
		return openEncrypted(ctx, in, passphrase)
	}
	if len(passphrase) != 0 {
		return nil, ErrPassphraseRequired
	}

	prefixReader := io.MultiReader(bytesReader(prefix[:n]), in)
	return openRaw(ctx, prefixReader)
}

func openRaw(ctx context.Context, in io.Reader) (Archive, error) {
	spool, err := os.CreateTemp("", "proxycore-backup-extract-*.zip")
	if err != nil {
		return nil, ErrCorruptArchive
	}
	if err := copyContext(ctx, spool, in); err != nil {
		_ = cleanupFile(spool)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, ErrCorruptArchive
	}
	return finalizeArchive(ctx, spool)
}

func openEncrypted(ctx context.Context, in io.Reader, passphrase []byte) (Archive, error) {
	var header [envelopeHeaderSize]byte
	copy(header[:4], envelopeMagic)
	if _, err := io.ReadFull(in, header[4:]); err != nil {
		return nil, ErrCorruptArchive
	}
	if header[4] != envelopeVersion {
		return nil, ErrUnsupportedVersion
	}
	if header[5] != envelopeKDFID || header[12] != envelopeReserved {
		return nil, ErrCorruptArchive
	}
	if binary.BigEndian.Uint32(header[6:10]) != kdfIterations ||
		header[10] != kdfRParam || header[11] != kdfPParam {
		return nil, ErrCorruptArchive
	}

	derivedKey, err := derivePassphraseKey(passphrase, header[13:29])
	if err != nil {
		return nil, ErrCorruptArchive
	}
	verifier := computeVerifier(derivedKey)
	matches := len(verifier) == len(header[41:73]) && subtle.ConstantTimeCompare(verifier, header[41:73]) == 1
	bodyKey := deriveBodyKey(derivedKey, header[29:41])
	wipe(derivedKey)
	wipe(verifier)
	if !matches {
		wipe(bodyKey)
		return nil, ErrPassphraseMismatch
	}

	spool, err := os.CreateTemp("", "proxycore-backup-extract-*.zip")
	if err != nil {
		wipe(bodyKey)
		return nil, ErrCorruptArchive
	}
	bodySize := binary.BigEndian.Uint64(header[73:81])
	decryptErr := decryptBody(ctx, in, bodySize, bodyKey, spool)
	wipe(bodyKey)
	if decryptErr != nil {
		_ = cleanupFile(spool)
		return nil, decryptErr
	}
	return finalizeArchive(ctx, spool)
}

func finalizeArchive(ctx context.Context, spool *os.File) (Archive, error) {
	stat, err := spool.Stat()
	if err != nil {
		_ = cleanupFile(spool)
		return nil, ErrCorruptArchive
	}
	reader, err := zip.NewReader(spool, stat.Size())
	if err != nil {
		_ = cleanupFile(spool)
		return nil, ErrCorruptArchive
	}

	files := make([]File, 0, len(reader.File))
	zipFiles := make([]*zip.File, 0, len(reader.File))
	seen := make(map[string]struct{}, len(reader.File))
	for index, entry := range reader.File {
		if err := contextErr(ctx); err != nil {
			_ = cleanupFile(spool)
			return nil, err
		}
		if err := ValidatePath(entry.Name); err != nil {
			_ = cleanupFile(spool)
			return nil, err
		}
		if _, exists := seen[entry.Name]; exists {
			_ = cleanupFile(spool)
			return nil, ErrDuplicatePath
		}
		seen[entry.Name] = struct{}{}
		if entry.UncompressedSize64 > uint64(maxExtractedFileSize) {
			_ = cleanupFile(spool)
			return nil, ErrFileTooLarge
		}
		if entry.Method != zip.Store && entry.Method != zip.Deflate {
			_ = cleanupFile(spool)
			return nil, ErrUnknownCompression
		}
		zipFiles = append(zipFiles, entry)
		entryIndex := index
		files = append(files, File{
			Path: entry.Name,
			Size: int64(entry.UncompressedSize64),
			Open: func() (io.ReadCloser, error) {
				return openEntry(spool, zipFiles, entryIndex)
			},
		})
	}
	return &archive{
		spool:    spool,
		name:     spool.Name(),
		files:    files,
		zipFiles: zipFiles,
	}, nil
}

type archive struct {
	mu       sync.RWMutex
	spool    *os.File
	name     string
	files    []File
	zipFiles []*zip.File
	closed   bool
	closeErr error
}

func (a *archive) Files(ctx context.Context) ([]File, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.closed {
		return nil, errArchiveClosed
	}
	files := make([]File, len(a.files))
	copy(files, a.files)
	return files, nil
}

func (a *archive) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return a.closeErr
	}
	a.closed = true
	closeErr := a.spool.Close()
	removeErr := os.Remove(a.name)
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	a.closeErr = errors.Join(closeErr, removeErr)
	return a.closeErr
}

func openEntry(spool *os.File, files []*zip.File, index int) (io.ReadCloser, error) {
	if spool == nil || index < 0 || index >= len(files) {
		return nil, errArchiveClosed
	}
	return files[index].Open()
}

func decryptBody(ctx context.Context, in io.Reader, bodySize uint64, bodyKey []byte, out io.Writer) error {
	if bodySize < 4 {
		return ErrTrailerMismatch
	}
	block, err := aes.NewCipher(bodyKey)
	if err != nil {
		return ErrCorruptArchive
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return ErrCorruptArchive
	}

	remaining := bodySize
	for {
		if err := contextErr(ctx); err != nil {
			return err
		}
		if remaining < 4 {
			return ErrTrailerMismatch
		}
		var sizeBytes [4]byte
		if err := readContextFull(ctx, in, sizeBytes[:]); err != nil {
			return ErrCorruptArchive
		}
		remaining -= 4
		chunkSize := binary.BigEndian.Uint32(sizeBytes[:])
		if chunkSize == 0 {
			if remaining != 0 {
				return ErrTrailerMismatch
			}
			break
		}
		if chunkSize > envelopeChunkSize {
			return ErrCorruptArchive
		}
		recordSize := uint64(envelopeNonceSize) + uint64(chunkSize) + envelopeTagSize
		if remaining < recordSize {
			return ErrTrailerMismatch
		}
		remaining -= recordSize
		chunkNonce := make([]byte, envelopeNonceSize)
		if err := readContextFull(ctx, in, chunkNonce); err != nil {
			return ErrCorruptArchive
		}
		sealed := make([]byte, int(chunkSize)+envelopeTagSize)
		if err := readContextFull(ctx, in, sealed); err != nil {
			return ErrCorruptArchive
		}
		plain, err := gcm.Open(nil, chunkNonce, sealed, nil)
		if err != nil || len(plain) != int(chunkSize) {
			return ErrCorruptArchive
		}
		if err := writeAll(out, plain); err != nil {
			return ErrCorruptArchive
		}
	}

	var trailing [1]byte
	n, err := in.Read(trailing[:])
	if n != 0 {
		return ErrTrailerMismatch
	}
	if err != io.EOF {
		if err == nil {
			return ErrTrailerMismatch
		}
		return ErrCorruptArchive
	}
	return nil
}

func derivePassphraseKey(passphrase, salt []byte) ([]byte, error) {
	return pbkdf2.Key(sha256.New, string(passphrase), salt, kdfIterations, 32)
}

func computeVerifier(key []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(verifierMessage))
	return mac.Sum(nil)
}

func deriveBodyKey(key, nonce []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(bodyKeyContext))
	_, _ = mac.Write(nonce)
	return mac.Sum(nil)
}

func wipe(data []byte) {
	for index := range data {
		data[index] = 0
	}
}

func writeAll(out io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := out.Write(data)
		if written > 0 {
			data = data[written:]
		}
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func copyContext(ctx context.Context, out io.Writer, in io.Reader) error {
	_, err := io.Copy(out, &contextReader{ctx: ctx, reader: in})
	return err
}

func readContextFull(ctx context.Context, in io.Reader, destination []byte) error {
	_, err := io.ReadFull(&contextReader{ctx: ctx, reader: in}, destination)
	return err
}

func contextErr(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := contextErr(r.ctx); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func cleanupFile(file *os.File) error {
	if file == nil {
		return nil
	}
	closeErr := file.Close()
	removeErr := os.Remove(file.Name())
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	return errors.Join(closeErr, removeErr)
}

func bytesReader(data []byte) io.Reader {
	return &byteReader{data: data}
}

type byteReader struct {
	data []byte
	off  int
}

func (r *byteReader) Read(p []byte) (int, error) {
	if r.off == len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}
