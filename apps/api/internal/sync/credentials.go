package sync

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	NodeCredentialPrefix           = "pcnode1"
	NodeCredentialSecretBytes      = 32
	NodeCredentialSecretEncodedLen = 43
	NodeCredentialIDLen            = 36
	NodeCredentialMaxBytes         = len(NodeCredentialPrefix) + 1 + NodeCredentialIDLen + 1 + NodeCredentialSecretEncodedLen

	// NodeCredentialHashVersion is persisted beside credential hashes. It is
	// intentionally different from the enrollment-token hash version.
	NodeCredentialHashVersion = "sha256-node-v1"
	// NodeCredentialHashDomain prevents a node bearer hash from being reused as
	// an enrollment-token hash if the same secret is ever presented to both APIs.
	NodeCredentialHashDomain = "proxycore/node-credential/"

	nodeCredentialRedacted = "[node credential redacted]"
)

var ErrNodeCredentialDenied = errors.New("node credential denied")

// NodeCredential owns a generated or parsed node bearer secret. Its secret is
// deliberately inaccessible as a string except through the explicit
// BearerCopy escape hatch used by a later transport boundary.
type NodeCredential struct {
	id     string
	secret []byte
}

// NodeCredentialRecord is the non-secret row projection needed for one current
// authentication check. Callers must load it afresh for every request.
type NodeCredentialRecord struct {
	ID          string     `json:"id"`
	NodeID      string     `json:"nodeId"`
	Hash        string     `json:"-"`
	HashVersion string     `json:"-"`
	RevokedAt   *time.Time `json:"revokedAt,omitempty"`
}

// NodePrincipal contains only identifiers safe for authorization context.
type NodePrincipal struct {
	NodeID       string `json:"nodeId"`
	CredentialID string `json:"credentialId"`
}

// GenerateNodeCredential creates a fresh UUID-backed bearer with 256 bits of
// CSPRNG secret material. The entropy reader is kept behind an unexported seam
// so failure and deterministic-vector behavior can be tested without changing
// the public API.
func GenerateNodeCredential() (NodeCredential, error) {
	return generateNodeCredentialWithEntropy(rand.Reader)
}

func generateNodeCredentialWithEntropy(entropy io.Reader) (NodeCredential, error) {
	if entropy == nil {
		return NodeCredential{}, ErrNodeCredentialDenied
	}
	id, err := uuid.NewRandomFromReader(entropy)
	if err != nil {
		return NodeCredential{}, ErrNodeCredentialDenied
	}
	secret := make([]byte, NodeCredentialSecretBytes)
	if _, err := io.ReadFull(entropy, secret); err != nil {
		zeroCredentialBytes(secret)
		return NodeCredential{}, ErrNodeCredentialDenied
	}
	credential, err := newNodeCredential(id.String(), secret)
	zeroCredentialBytes(secret)
	if err != nil {
		return NodeCredential{}, ErrNodeCredentialDenied
	}
	return credential, nil
}

// NewNodeCredential copies exactly one raw bearer secret into an owned
// wrapper. It is intended for the sealed-bootstrap boundary, not durable
// storage; callers should destroy the returned wrapper after wrapping the
// secret locally.
func NewNodeCredential(id string, secret []byte) (NodeCredential, error) {
	return newNodeCredential(id, secret)
}

func newNodeCredential(id string, secret []byte) (NodeCredential, error) {
	canonicalID, ok := canonicalCredentialID(id)
	if !ok || len(secret) != NodeCredentialSecretBytes {
		return NodeCredential{}, ErrNodeCredentialDenied
	}
	return NodeCredential{id: canonicalID, secret: append([]byte(nil), secret...)}, nil
}

// ParseNodeCredential strictly parses the complete pcnode1 bearer format.
func ParseNodeCredential(value string) (NodeCredential, error) {
	if len(value) > NodeCredentialMaxBytes || len(value) != NodeCredentialMaxBytes || !utf8.ValidString(value) {
		return NodeCredential{}, ErrNodeCredentialDenied
	}
	for _, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return NodeCredential{}, ErrNodeCredentialDenied
		}
	}
	prefixEnd := len(NodeCredentialPrefix)
	idStart := prefixEnd + 1
	idEnd := idStart + NodeCredentialIDLen
	secretStart := idEnd + 1
	if value[:prefixEnd] != NodeCredentialPrefix || value[prefixEnd] != '_' || value[idEnd] != '_' {
		return NodeCredential{}, ErrNodeCredentialDenied
	}
	canonicalID, ok := canonicalCredentialID(value[idStart:idEnd])
	if !ok {
		return NodeCredential{}, ErrNodeCredentialDenied
	}
	encodedSecret := value[secretStart:]
	secret, err := base64.RawURLEncoding.DecodeString(encodedSecret)
	if err != nil || len(secret) != NodeCredentialSecretBytes || base64.RawURLEncoding.EncodeToString(secret) != encodedSecret {
		zeroCredentialBytes(secret)
		return NodeCredential{}, ErrNodeCredentialDenied
	}
	return NodeCredential{id: canonicalID, secret: secret}, nil
}

// ID returns the non-secret credential identifier.
func (c NodeCredential) ID() string { return c.id }

// CredentialID returns the non-secret credential identifier.
func (c NodeCredential) CredentialID() string { return c.id }

// CopyBytes returns an independent copy of the raw 32-byte bearer secret.
func (c NodeCredential) CopyBytes() []byte {
	if len(c.secret) != NodeCredentialSecretBytes {
		return nil
	}
	return append([]byte(nil), c.secret...)
}

// BearerCopy explicitly creates the complete plaintext bearer for a transport
// boundary. A destroyed or invalid wrapper cannot create a bearer.
func (c NodeCredential) BearerCopy() string {
	if len(c.secret) != NodeCredentialSecretBytes {
		return ""
	}
	return NodeCredentialPrefix + "_" + c.id + "_" + base64.RawURLEncoding.EncodeToString(c.secret)
}

// Hash returns the canonical lowercase SHA-256 hash for DB persistence. Only
// the secret is hashed; the credential ID is lookup material and is not part
// of the digest.
func (c NodeCredential) Hash() string {
	if len(c.secret) != NodeCredentialSecretBytes {
		return ""
	}
	digest := nodeCredentialDigest(c.secret)
	return hex.EncodeToString(digest[:])
}

// CredentialHash is an explicit alias for callers persisting a credential row.
func (c NodeCredential) CredentialHash() string { return c.Hash() }

// Destroy zeroes the owned secret. The non-secret ID remains available for
// audit/association without allowing bearer reconstruction.
func (c *NodeCredential) Destroy() {
	if c == nil {
		return
	}
	zeroCredentialBytes(c.secret)
	c.secret = nil
}

// AuthenticateNodeCredential validates one freshly loaded credential record.
// Every malformed, missing, revoked, mismatched, or unsupported input returns
// the same generic sentinel and no secret-bearing value.
func AuthenticateNodeCredential(presented string, record *NodeCredentialRecord) (NodePrincipal, error) {
	credential, err := ParseNodeCredential(presented)
	if err != nil || record == nil {
		return NodePrincipal{}, ErrNodeCredentialDenied
	}
	defer credential.Destroy()
	storedID, ok := canonicalCredentialID(record.ID)
	if !ok || storedID != credential.id {
		return NodePrincipal{}, ErrNodeCredentialDenied
	}
	storedNodeID, ok := canonicalCredentialID(record.NodeID)
	if !ok {
		return NodePrincipal{}, ErrNodeCredentialDenied
	}
	if record.HashVersion != NodeCredentialHashVersion || record.RevokedAt != nil {
		return NodePrincipal{}, ErrNodeCredentialDenied
	}
	storedHash, ok := canonicalCredentialHash(record.Hash)
	if !ok {
		return NodePrincipal{}, ErrNodeCredentialDenied
	}
	actualHash := nodeCredentialDigest(credential.secret)
	if subtle.ConstantTimeCompare(storedHash[:], actualHash[:]) != 1 {
		return NodePrincipal{}, ErrNodeCredentialDenied
	}
	return NodePrincipal{NodeID: storedNodeID, CredentialID: credential.id}, nil
}

func canonicalCredentialID(value string) (string, bool) {
	if len(value) != NodeCredentialIDLen || !utf8.ValidString(value) {
		return "", false
	}
	parsed, err := uuid.Parse(value)
	if err != nil || parsed == uuid.Nil || parsed.String() != value {
		return "", false
	}
	return parsed.String(), true
}

func canonicalCredentialHash(value string) ([sha256.Size]byte, bool) {
	var result [sha256.Size]byte
	if len(value) != sha256.Size*2 || !utf8.ValidString(value) {
		return result, false
	}
	for _, character := range []byte(value) {
		if !isLowerHex(character) {
			return result, false
		}
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return result, false
	}
	copy(result[:], decoded)
	return result, true
}

func isLowerHex(value byte) bool {
	return (value >= '0' && value <= '9') || (value >= 'a' && value <= 'f')
}

func nodeCredentialDigest(secret []byte) [sha256.Size]byte {
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(NodeCredentialHashDomain))
	_, _ = hasher.Write(secret)
	var result [sha256.Size]byte
	copy(result[:], hasher.Sum(nil))
	return result
}

func zeroCredentialBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func (c NodeCredential) String() string { return nodeCredentialRedacted }

func (c NodeCredential) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, nodeCredentialRedacted)
}

func (c NodeCredential) MarshalJSON() ([]byte, error) {
	return json.Marshal(nodeCredentialRedacted)
}

func (c NodeCredential) MarshalText() ([]byte, error) {
	return []byte(nodeCredentialRedacted), nil
}
