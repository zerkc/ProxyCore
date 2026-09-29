package dbexport

import "io"

// ZipWriter is the narrow output port used by Export. A negative size means
// that the entry is streamed and its final size is not known before opening.
type ZipWriter interface {
	Add(path string, size int64, open func() (io.ReadCloser, error)) error
}
