package httpserver

import (
	"net/http"
	"strings"
)

// nodeReadOnlyExemptions is the single place that names every mutating route
// that a non-writable identity (NODE or stale PRIMARY) may still call. Any
// other mutating request that authenticates through requireUser or
// requireAuthenticatedUser is rejected with TOPOLOGY_READ_ONLY, so a newly
// added mutating route is read-only on a NODE by default.
//
// Keys are "METHOD path". Routes under EnrollmentTokensPath+"/{id}/revoke" are
// matched by prefix in nodeReadOnlyExempt.
var nodeReadOnlyExemptions = map[string]struct{}{
	// Software update: a NODE must be able to apply an update.
	http.MethodPost + " /api/updates/apply": {},
	// Backup export only reads state; import stays blocked.
	http.MethodPost + " /api/backup/export": {},
	// Authentication and session management do not touch configuration.
	http.MethodPost + " /api/auth/bootstrap":       {},
	http.MethodPost + " /api/auth/login":           {},
	http.MethodPost + " /api/auth/logout":          {},
	http.MethodPost + " /api/auth/change-password": {},
	// Topology transitions and sync recovery enforce their own identity rules.
	http.MethodPost + " " + TopologyPromotePath:   {},
	http.MethodPost + " " + TopologyDemotePath:    {},
	http.MethodPost + " " + TopologySyncRetryPath: {},
	// Enrollment workflow and token management enforce their own identity rules.
	http.MethodPost + " " + EnrollmentTokensPath:  {},
	http.MethodPost + " " + EnrollmentDraftPath:   {},
	http.MethodPost + " " + EnrollmentPreviewPath: {},
	http.MethodPost + " " + EnrollmentConfirmPath: {},
	http.MethodPost + " " + EnrollmentRecoverPath: {},
}

// nodeReadOnlyExempt reports whether a mutating request is allowed to proceed
// on a non-writable identity.
func nodeReadOnlyExempt(r *http.Request) bool {
	path := r.URL.Path
	if _, ok := nodeReadOnlyExemptions[r.Method+" "+path]; ok {
		return true
	}
	return r.Method == http.MethodPost &&
		strings.HasPrefix(path, EnrollmentTokensPath+"/") &&
		strings.HasSuffix(path, "/revoke")
}
