package httpexport

import (
	"crypto/sha256"
	"encoding/hex"
)

// staticIdentitySource is a fixed IdentitySource for bundle tests.
type staticIdentitySource struct{}

func (staticIdentitySource) InstallationIDString() string { return "installation-test" }
func (staticIdentitySource) NodeIDString() string         { return "node-test" }
func (staticIdentitySource) RoleString() string           { return "standalone-primary" }

func manifestDigestForTest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
