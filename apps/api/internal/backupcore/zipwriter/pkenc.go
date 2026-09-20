package zipwriter

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
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
	envelopeVerifierSize = sha256.Size
	envelopeHeaderSize   = 81
	envelopeChunkSize    = 64 * 1024
	envelopeTagSize      = 16
	bodyKeyContext       = "proxycore-backup-body-v1"
	verifierMessage      = "proxycore-backup-v1"
)

var (
	errEncryptedWriterClosed = errors.New("zipwriter: encrypted writer closed")
	errEncryptedBodySize     = errors.New("zipwriter: encrypted body size mismatch")
)

// OpenEncrypted wraps the ZIP stream in the BX01 envelope when passphrase is
// non-empty. The ZIP is spooled to a 0600 temporary file so body_size can be
// emitted before the encrypted body without retaining the archive in memory.
func OpenEncrypted(out io.Writer, passphrase []byte, opts ...WriterOption) (Writer, error) {
	if len(passphrase) == 0 {
		return Open(out, opts...), nil
	}
	if out == nil {
		out = io.Discard
	}

	salt := make([]byte, envelopeSaltSize)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, fmt.Errorf("zipwriter: generate encryption salt: %w", err)
	}
	headerNonce := make([]byte, envelopeNonceSize)
	if _, err := io.ReadFull(rand.Reader, headerNonce); err != nil {
		return nil, fmt.Errorf("zipwriter: generate encryption nonce: %w", err)
	}
	derivedKey, err := derivePassphraseKey(passphrase, salt)
	if err != nil {
		return nil, fmt.Errorf("zipwriter: derive encryption key: %w", err)
	}
	verifier := computeVerifier(derivedKey)
	bodyKey := deriveBodyKey(derivedKey, headerNonce)
	wipe(derivedKey)

	spool, err := os.CreateTemp("", "proxycore-backup-*.zip")
	if err != nil {
		wipe(bodyKey)
		return nil, fmt.Errorf("zipwriter: create archive spool: %w", err)
	}
	raw := Open(noCloseWriter{Writer: spool}, opts...)
	return &encryptedWriter{
		out:         out,
		spool:       spool,
		archive:     raw,
		salt:        salt,
		headerNonce: headerNonce,
		verifier:    verifier,
		bodyKey:     bodyKey,
	}, nil
}

type encryptedWriter struct {
	out         io.Writer
	spool       *os.File
	archive     Writer
	salt        []byte
	headerNonce []byte
	verifier    []byte
	bodyKey     []byte
	firstErr    error
	closed      bool
	closeErr    error
}

func (w *encryptedWriter) Add(path string, size int64, open func() (io.ReadCloser, error)) error {
	if w.closed {
		return errEncryptedWriterClosed
	}
	err := w.archive.Add(path, size, open)
	if err != nil && w.firstErr == nil {
		w.firstErr = err
	}
	return err
}

func (w *encryptedWriter) Close() error {
	if w.closed {
		return w.closeErr
	}
	w.closed = true

	archiveErr := w.archive.Close()
	if w.firstErr != nil {
		archiveErr = errors.Join(w.firstErr, archiveErr)
	}
	if archiveErr != nil {
		w.closeErr = archiveErr
	} else {
		w.closeErr = w.writeEnvelope()
	}
	w.closeErr = errors.Join(w.closeErr, closeWriter(w.out), w.cleanup())
	wipe(w.salt)
	wipe(w.headerNonce)
	wipe(w.verifier)
	wipe(w.bodyKey)
	return w.closeErr
}

func (w *encryptedWriter) writeEnvelope() error {
	stat, err := w.spool.Stat()
	if err != nil {
		return fmt.Errorf("zipwriter: inspect archive spool: %w", err)
	}
	bodySize, err := encryptedBodySize(stat.Size())
	if err != nil {
		return err
	}
	if _, err := w.spool.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("zipwriter: rewind archive spool: %w", err)
	}

	header := makeEnvelopeHeader(w.salt, w.headerNonce, w.verifier, bodySize)
	if err := writeAll(w.out, header); err != nil {
		return fmt.Errorf("zipwriter: write encryption header: %w", err)
	}
	block, err := aes.NewCipher(w.bodyKey)
	if err != nil {
		return fmt.Errorf("zipwriter: initialize encryption cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("zipwriter: initialize encryption mode: %w", err)
	}

	var plain [envelopeChunkSize]byte
	var written uint64
	for {
		n, readErr := io.ReadFull(w.spool, plain[:])
		if readErr == io.EOF && n == 0 {
			break
		}
		if readErr != nil && readErr != io.ErrUnexpectedEOF {
			return fmt.Errorf("zipwriter: read archive spool: %w", readErr)
		}
		if n == 0 {
			break
		}

		chunkNonce := make([]byte, envelopeNonceSize)
		if _, err := io.ReadFull(rand.Reader, chunkNonce); err != nil {
			return fmt.Errorf("zipwriter: generate chunk nonce: %w", err)
		}
		sealed := gcm.Seal(nil, chunkNonce, plain[:n], nil)
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(n))
		if err := writeAll(w.out, size[:]); err != nil {
			return fmt.Errorf("zipwriter: write encrypted chunk: %w", err)
		}
		if err := writeAll(w.out, chunkNonce); err != nil {
			return fmt.Errorf("zipwriter: write encrypted chunk nonce: %w", err)
		}
		if err := writeAll(w.out, sealed); err != nil {
			return fmt.Errorf("zipwriter: write encrypted chunk body: %w", err)
		}
		written += uint64(4 + envelopeNonceSize + len(sealed))
		if readErr == io.ErrUnexpectedEOF {
			break
		}
	}

	var terminator [4]byte
	if err := writeAll(w.out, terminator[:]); err != nil {
		return fmt.Errorf("zipwriter: write encrypted trailer: %w", err)
	}
	written += uint64(len(terminator))
	if written != bodySize {
		return errEncryptedBodySize
	}
	return nil
}

func (w *encryptedWriter) cleanup() error {
	var errs []error
	if w.spool != nil {
		if err := w.spool.Close(); err != nil {
			errs = append(errs, err)
		}
		if err := os.Remove(w.spool.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func derivePassphraseKey(passphrase, salt []byte) ([]byte, error) {
	// 600,000 SHA-256 rounds keeps offline guessing materially expensive while
	// remaining a short, defensible delay on commodity API hardware.
	return pbkdf2.Key(sha256.New, string(passphrase), salt, kdfIterations, 32)
}

func constantTimeEqual(left, right []byte) bool {
	return len(left) == len(right) && subtle.ConstantTimeCompare(left, right) == 1
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

func encryptedBodySize(plainSize int64) (uint64, error) {
	if plainSize < 0 {
		return 0, errEncryptedBodySize
	}
	chunks := uint64(plainSize / envelopeChunkSize)
	if plainSize%envelopeChunkSize != 0 {
		chunks++
	}
	return uint64(plainSize) + chunks*uint64(4+envelopeNonceSize+envelopeTagSize) + 4, nil
}

func makeEnvelopeHeader(salt, nonce, verifier []byte, bodySize uint64) []byte {
	header := make([]byte, envelopeHeaderSize)
	copy(header[0:4], envelopeMagic)
	header[4] = envelopeVersion
	header[5] = envelopeKDFID
	binary.BigEndian.PutUint32(header[6:10], kdfIterations)
	header[10] = kdfRParam
	header[11] = kdfPParam
	header[12] = envelopeReserved
	copy(header[13:29], salt)
	copy(header[29:41], nonce)
	copy(header[41:73], verifier)
	binary.BigEndian.PutUint64(header[73:81], bodySize)
	return header
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

func wipe(data []byte) {
	for index := range data {
		data[index] = 0
	}
}

type noCloseWriter struct {
	io.Writer
}
