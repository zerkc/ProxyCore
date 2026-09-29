package hostread

import "errors"

// ErrNoAppliedRevision reports that the configuration store has no applied revision.
var ErrNoAppliedRevision = errors.New("hostread: no applied revision")
