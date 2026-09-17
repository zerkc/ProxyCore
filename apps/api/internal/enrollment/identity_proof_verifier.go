package enrollment

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

type IdentityProofVerifierOptions struct {
	Request                      IdentityProofRequest
	ExpectedInstallationID       domain.InstallationID
	ExpectedNodeID               domain.NodeID
	ExpectedLeadershipGeneration domain.LeadershipGeneration
	ExpectedLeafSPKISHA256       string
	ExpectedCADERHashSHA256      string
	LeafCertificatePEM           string
	CACertificatePEM             string
	Now                          time.Time
}

func VerifyIdentityProof(proof IdentityProof, opts IdentityProofVerifierOptions) error {
	if err := proof.validateShape(); err != nil {
		return err
	}
	if err := opts.Request.validate(); err != nil ||
		proof.AttemptID != opts.Request.AttemptID ||
		proof.Nonce != opts.Request.Nonce || proof.PrimaryURL != opts.Request.PrimaryURL {
		return ErrIdentityProofBinding
	}
	if !opts.ExpectedInstallationID.IsValid() || !opts.ExpectedNodeID.IsValid() ||
		proof.InstallationID != opts.ExpectedInstallationID.String() ||
		proof.NodeID != opts.ExpectedNodeID.String() ||
		opts.ExpectedLeadershipGeneration == 0 ||
		proof.LeadershipGeneration != uint64(opts.ExpectedLeadershipGeneration) {
		return ErrIdentityProofBinding
	}
	leaf, err := parseCertificate(opts.LeafCertificatePEM)
	if err != nil {
		return err
	}
	ca, err := parseCertificate(opts.CACertificatePEM)
	if err != nil {
		return err
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	if err := validateCertificatePair(leaf, ca, now); err != nil {
		return err
	}
	if err := certificateMatchesURL(leaf, proof.PrimaryURL); err != nil {
		return err
	}
	if !proof.LeafNotBefore.Equal(leaf.NotBefore.UTC()) ||
		!proof.LeafNotAfter.Equal(leaf.NotAfter.UTC()) ||
		proof.LeafSPKISHA256 != publicKeyHash(leaf.PublicKey) ||
		proof.CADERHashSHA256 != derHash(ca.Raw) {
		return ErrIdentityProofBinding
	}
	if opts.ExpectedLeafSPKISHA256 != "" && proof.LeafSPKISHA256 != opts.ExpectedLeafSPKISHA256 ||
		opts.ExpectedCADERHashSHA256 != "" && proof.CADERHashSHA256 != opts.ExpectedCADERHashSHA256 {
		return ErrIdentityProofBinding
	}
	if now.Before(proof.IssuedAt) || !now.Before(proof.ExpiresAt) ||
		proof.ExpiresAt.Sub(proof.IssuedAt) > IdentityProofLifetime ||
		proof.IssuedAt.Before(leaf.NotBefore) || !proof.ExpiresAt.Before(leaf.NotAfter) {
		return ErrIdentityProofTimeWindow
	}
	signature, err := decodeSignature(proof.Signature)
	if err != nil {
		return err
	}
	publicKey, ok := leaf.PublicKey.(*rsa.PublicKey)
	if !ok {
		return ErrUnsupportedIdentityProofKey
	}
	canonical, err := proof.CanonicalBytes()
	if err != nil {
		return err
	}
	digest := sha256.Sum256(canonical)
	if rsa.VerifyPKCS1v15(publicKey, crypto.SHA256, digest[:], signature) != nil {
		return ErrIdentityProofBinding
	}
	return nil
}

func (p IdentityProof) validateShape() error {
	if p.Version != IdentityProofVersion {
		return ErrInvalidIdentityProof
	}
	if p.SignatureAlgorithm != IdentityProofSignatureAlgorithm {
		return ErrUnsupportedIdentityProofAlgorithm
	}
	canonical, err := CanonicalizePrimaryURL(p.PrimaryURL)
	if !validUUIDv4(p.AttemptID) || !validNonce(p.Nonce) || err != nil ||
		canonical != p.PrimaryURL || !domain.InstallationID(p.InstallationID).IsValid() ||
		!domain.NodeID(p.NodeID).IsValid() ||
		(p.Role != string(domain.TopologyRolePrimary) && p.Role != string(domain.TopologyRolePrimaryWithNodes)) ||
		p.LeadershipGeneration == 0 || !validHash(p.LeafSPKISHA256) ||
		!validHash(p.CADERHashSHA256) || !canonicalTime(p.LeafNotBefore) ||
		!canonicalTime(p.LeafNotAfter) || !canonicalTime(p.IssuedAt) ||
		!canonicalTime(p.ExpiresAt) || !p.LeafNotAfter.After(p.LeafNotBefore) ||
		!p.ExpiresAt.After(p.IssuedAt) || p.ExpiresAt.Sub(p.IssuedAt) > IdentityProofLifetime {
		return ErrInvalidIdentityProof
	}
	return nil
}
