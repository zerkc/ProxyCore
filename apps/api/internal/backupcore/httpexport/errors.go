package httpexport

import "errors"

// ErrBodyTooLarge reports an import body larger than the configured limit.
var ErrBodyTooLarge = errors.New("httpexport: body exceeds configured limit")
