package zipwriter

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"

	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/zipextract"
)

const maxFileSize int64 = 2 << 30

var (
	errWriterClosed = errors.New("zipwriter: writer closed")
	errNilOpener    = errors.New("zipwriter: nil opener")
)

// Writer streams bundle files into a ZIP archive.
type Writer interface {
	Add(path string, size int64, open func() (io.ReadCloser, error)) error
	Close() error
}

type writer struct {
	out      io.Writer
	archive  *zip.Writer
	options  writerOptions
	seen     map[string]struct{}
	total    int64
	initErr  error
	closed   bool
	closeErr error
}

// Open creates a streaming ZIP writer. The returned writer uses DEFLATE by
// default and does not retain file contents between Add calls.
func Open(out io.Writer, opts ...WriterOption) Writer {
	options := defaultWriterOptions()
	for _, option := range opts {
		if option != nil {
			option(&options)
		}
	}
	if out == nil {
		out = io.Discard
	}

	archive := zip.NewWriter(out)
	result := &writer{
		out:     out,
		archive: archive,
		options: options,
		seen:    make(map[string]struct{}),
	}
	if options.comment != "" {
		if err := archive.SetComment(options.comment); err != nil {
			result.initErr = err
		}
	}
	return result
}

func (w *writer) Add(name string, size int64, open func() (io.ReadCloser, error)) error {
	if w.closed {
		return errWriterClosed
	}
	if w.initErr != nil {
		return w.initErr
	}
	if err := zipextract.ValidatePath(name); err != nil {
		return err
	}
	if _, exists := w.seen[name]; exists {
		return zipextract.ErrDuplicatePath
	}
	if size > maxFileSize {
		return zipextract.ErrFileTooLarge
	}
	if open == nil {
		return fmt.Errorf("zipwriter: open %q: %w", name, errNilOpener)
	}
	if w.options.aggregateSet && size >= 0 && exceeds(w.total, size, w.options.aggregateLimit) {
		return zipextract.ErrAggregateLimitExceeded
	}
	if w.options.compression != zip.Store && w.options.compression != zip.Deflate {
		return zipextract.ErrUnknownCompression
	}

	body, err := w.archive.CreateHeader(&zip.FileHeader{Name: name, Method: w.options.compression})
	if err != nil {
		return err
	}
	w.seen[name] = struct{}{}

	reader, openErr := open()
	if openErr != nil {
		if reader != nil {
			_ = reader.Close()
		}
		return fmt.Errorf("zipwriter: open %q: %w", name, openErr)
	}
	if reader == nil {
		return fmt.Errorf("zipwriter: open %q: %w", name, errNilOpener)
	}

	limit := maxFileSize
	limitErr := zipextract.ErrFileTooLarge
	if w.options.aggregateSet {
		remaining := w.options.aggregateLimit - w.total
		if remaining < 0 {
			remaining = 0
		}
		if remaining < limit {
			limit = remaining
			limitErr = zipextract.ErrAggregateLimitExceeded
		}
	}
	bounded := &boundedReader{source: reader, limit: limit, limitErr: limitErr}
	_, copyErr := io.Copy(body, bounded)
	closeErr := reader.Close()
	w.total += bounded.count
	if copyErr != nil || closeErr != nil {
		return errors.Join(copyErr, closeErr)
	}
	return nil
}

func (w *writer) Close() error {
	if w.closed {
		return w.closeErr
	}
	w.closed = true
	w.closeErr = errors.Join(w.initErr, w.archive.Close(), closeWriter(w.out))
	return w.closeErr
}

func exceeds(current, addition, limit int64) bool {
	if current > limit {
		return true
	}
	return addition > limit-current
}

func closeWriter(out io.Writer) error {
	closer, ok := out.(io.Closer)
	if !ok {
		return nil
	}
	return closer.Close()
}

type boundedReader struct {
	source   io.Reader
	limit    int64
	limitErr error
	count    int64
}

func (r *boundedReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.count >= r.limit {
		var probe [1]byte
		n, err := r.source.Read(probe[:])
		if n > 0 {
			return 0, r.limitErr
		}
		if err == io.EOF {
			return 0, io.EOF
		}
		if err != nil {
			return 0, err
		}
		return 0, io.ErrNoProgress
	}
	remaining := r.limit - r.count
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := r.source.Read(p)
	r.count += int64(n)
	return n, err
}
