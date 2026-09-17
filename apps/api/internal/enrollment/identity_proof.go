package enrollment

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"time"

	"github.com/google/uuid"
)

const (
	IdentityProofVersion            uint32 = 1
	IdentityProofSignatureAlgorithm        = "rsa-pkcs1-sha256"
	IdentityProofLifetime                  = 2 * time.Minute
	identityProofNonceBytes                = 32
	maxIdentityProofStringBytes            = 2048
)

var (
	ErrInvalidIdentityProofRequest       = errors.New("invalid identity proof request")
	ErrInvalidIdentityProof              = errors.New("invalid identity proof")
	ErrIdentityProofIdentityNotLoaded    = errors.New("identity is not loaded")
	ErrIdentityProofIdentityNotEligible  = errors.New("identity is not eligible to sign identity proofs")
	ErrIdentityProofGeneration           = errors.New("invalid leadership generation")
	ErrUnsupportedIdentityProofKey       = errors.New("unsupported identity proof key")
	ErrUnsupportedIdentityProofAlgorithm = errors.New("unsupported identity proof signature algorithm")
	ErrIdentityProofTimeWindow           = errors.New("identity proof is outside its validity window")
	ErrIdentityProofBinding              = errors.New("identity proof binding mismatch")
	ErrInvalidIdentityProofProvider      = errors.New("invalid identity proof provider")
)

type IdentityProofRequest struct {
	AttemptID  string `json:"attemptId"`
	Nonce      string `json:"nonce"`
	PrimaryURL string `json:"primaryUrl"`
}

type IdentityProof struct {
	Version              uint32    `json:"version"`
	SignatureAlgorithm   string    `json:"signatureAlgorithm"`
	AttemptID            string    `json:"attemptId"`
	Nonce                string    `json:"nonce"`
	PrimaryURL           string    `json:"primaryUrl"`
	InstallationID       string    `json:"installationId"`
	NodeID               string    `json:"nodeId"`
	Role                 string    `json:"role"`
	LeadershipGeneration uint64    `json:"leadershipGeneration"`
	LeafSPKISHA256       string    `json:"leafSpkiSha256"`
	CADERHashSHA256      string    `json:"caDerSha256"`
	LeafNotBefore        time.Time `json:"leafNotBefore"`
	LeafNotAfter         time.Time `json:"leafNotAfter"`
	IssuedAt             time.Time `json:"issuedAt"`
	ExpiresAt            time.Time `json:"expiresAt"`
	Signature            string    `json:"signature"`
}

func NewIdentityProofRequest(primaryURL string) (IdentityProofRequest, error) {
	if canonical, err := CanonicalizePrimaryURL(primaryURL); err != nil || canonical != primaryURL {
		return IdentityProofRequest{}, ErrInvalidIdentityProofRequest
	}
	attempt, err := uuid.NewRandom()
	if err != nil {
		return IdentityProofRequest{}, ErrInvalidIdentityProofRequest
	}
	nonce := make([]byte, identityProofNonceBytes)
	if _, err = rand.Read(nonce); err != nil {
		return IdentityProofRequest{}, ErrInvalidIdentityProofRequest
	}
	return IdentityProofRequest{attempt.String(), base64.RawURLEncoding.EncodeToString(nonce), primaryURL}, nil
}

func (r IdentityProofRequest) Validate() error { return r.validate() }

func (r IdentityProofRequest) validate() error {
	canonical, err := CanonicalizePrimaryURL(r.PrimaryURL)
	if !validUUIDv4(r.AttemptID) || !validNonce(r.Nonce) || err != nil || canonical != r.PrimaryURL {
		return ErrInvalidIdentityProofRequest
	}
	return nil
}

func (p IdentityProof) CanonicalBytes() ([]byte, error) {
	return canonicalIdentityProofBytes(p)
}

func CanonicalIdentityProofBytes(p IdentityProof) ([]byte, error) {
	return p.CanonicalBytes()
}

func canonicalIdentityProofBytes(p IdentityProof) ([]byte, error) {
	if err := p.validateShape(); err != nil {
		return nil, err
	}
	b := make([]byte, 0, 512)
	var err error
	for _, value := range []string{
		"proxycore/enrollment-identity-proof/v1", p.SignatureAlgorithm,
		p.AttemptID, p.Nonce, p.PrimaryURL, p.InstallationID, p.NodeID,
		p.Role, p.LeafSPKISHA256, p.CADERHashSHA256,
	} {
		b, err = appendLengthPrefixed(b, value)
		if err != nil {
			return nil, err
		}
	}
	var fixed [8]byte
	binary.BigEndian.PutUint32(fixed[:4], p.Version)
	b = append(b, fixed[:4]...)
	binary.BigEndian.PutUint64(fixed[:], p.LeadershipGeneration)
	b = append(b, fixed[:]...)
	for _, value := range []time.Time{p.LeafNotBefore, p.LeafNotAfter, p.IssuedAt, p.ExpiresAt} {
		binary.BigEndian.PutUint64(fixed[:], uint64(value.Unix()))
		b = append(b, fixed[:]...)
		binary.BigEndian.PutUint32(fixed[:4], uint32(value.Nanosecond()))
		b = append(b, fixed[:4]...)
	}
	return b, nil
}

func appendLengthPrefixed(dst []byte, value string) ([]byte, error) {
	if len(value) > maxIdentityProofStringBytes {
		return nil, ErrInvalidIdentityProof
	}
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	return append(append(dst, length[:]...), value...), nil
}
