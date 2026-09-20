package zipwriter

import "archive/zip"

// WriterOption configures an archive writer before its first entry is added.
type WriterOption func(*writerOptions)

type writerOptions struct {
	compression    uint16
	aggregateLimit int64
	aggregateSet   bool
	comment        string
}

func defaultWriterOptions() writerOptions {
	return writerOptions{compression: zip.Deflate}
}

// WithCompression selects a standard archive/zip compression method. Deflate
// is the default; Store is also supported for callers that need no compression.
func WithCompression(method uint16) WriterOption {
	return func(options *writerOptions) {
		options.compression = method
	}
}

// WithAggregateLimit limits the total uncompressed bytes accepted by the
// writer. A zero limit permits only empty files; a negative value means no
// limit.
func WithAggregateLimit(maxBytes int64) WriterOption {
	return func(options *writerOptions) {
		if maxBytes < 0 {
			options.aggregateSet = false
			options.aggregateLimit = 0
			return
		}
		options.aggregateSet = true
		options.aggregateLimit = maxBytes
	}
}

// WithComment records a ZIP archive comment.
func WithComment(comment string) WriterOption {
	return func(options *writerOptions) {
		options.comment = comment
	}
}
