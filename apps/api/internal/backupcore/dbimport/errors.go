package dbimport

import "errors"

var (
	ErrChecksumMismatch        = errors.New("backup checksum mismatch")
	ErrImportAlreadyInProgress = errors.New("backup import already in progress")
	ErrInvalidEnvMode          = errors.New("invalid backup environment file mode")
)
