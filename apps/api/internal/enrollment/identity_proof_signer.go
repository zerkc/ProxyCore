package enrollment

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
)

type IdentityProofIdentityProvider func(context.Context) (identity.Identity, bool, error)
type IdentityProofMaterialProvider func(context.Context) (leafCertificatePEM, leafPrivateKeyPEM, caCertificatePEM string, err error)

type IdentityProofSignerOptions struct {
	Identity IdentityProofIdentityProvider
	Material IdentityProofMaterialProvider
	Now      func() time.Time
}

type IdentityProofSigner struct {
	identity IdentityProofIdentityProvider
	material IdentityProofMaterialProvider
	now      func() time.Time
}

func NewIdentityProofSigner(opts IdentityProofSignerOptions) (*IdentityProofSigner, error) {
	if opts.Identity == nil || opts.Material == nil {
		return nil, ErrInvalidIdentityProofProvider
	}
	clock := opts.Now
	if clock == nil {
		clock = time.Now
	}
	return &IdentityProofSigner{identity: opts.Identity, material: opts.Material, now: clock}, nil
}

func (s *IdentityProofSigner) Sign(ctx context.Context, request IdentityProofRequest) (IdentityProof, error) {
	if s == nil || s.identity == nil || s.material == nil || s.now == nil {
		return IdentityProof{}, ErrInvalidIdentityProofProvider
	}
	if err := request.validate(); err != nil {
		return IdentityProof{}, err
	}
	current, loaded, err := s.identity(ctx)
	if err != nil {
		return IdentityProof{}, ErrInvalidIdentityProofProvider
	}
	if !loaded {
		return IdentityProof{}, ErrIdentityProofIdentityNotLoaded
	}
	if err := validateSigningIdentity(current); err != nil {
		return IdentityProof{}, err
	}
	leafPEM, leafKeyPEM, caPEM, err := s.material(ctx)
	if err != nil {
		return IdentityProof{}, ErrInvalidIdentityProofProvider
	}
	leaf, err := parseCertificate(leafPEM)
	if err != nil {
		return IdentityProof{}, err
	}
	ca, err := parseCertificate(caPEM)
	if err != nil {
		return IdentityProof{}, err
	}
	key, err := parseRSAKey(leafKeyPEM)
	if err != nil {
		return IdentityProof{}, err
	}
	if !samePublicKey(leaf.PublicKey, &key.PublicKey) {
		return IdentityProof{}, ErrInvalidIdentityProof
	}
	now := s.now().UTC()
	if err := validateCertificatePair(leaf, ca, now); err != nil {
		return IdentityProof{}, err
	}
	if err := certificateMatchesURL(leaf, request.PrimaryURL); err != nil {
		return IdentityProof{}, err
	}
	proof := IdentityProof{
		Version:              IdentityProofVersion,
		SignatureAlgorithm:   IdentityProofSignatureAlgorithm,
		AttemptID:            request.AttemptID,
		Nonce:                request.Nonce,
		PrimaryURL:           request.PrimaryURL,
		InstallationID:       current.InstallationID.String(),
		NodeID:               current.NodeID.String(),
		Role:                 string(current.Role),
		LeadershipGeneration: uint64(current.LeadershipGeneration),
		LeafSPKISHA256:       publicKeyHash(leaf.PublicKey),
		CADERHashSHA256:      derHash(ca.Raw),
		LeafNotBefore:        leaf.NotBefore.UTC(),
		LeafNotAfter:         leaf.NotAfter.UTC(),
		IssuedAt:             now,
		ExpiresAt:            now.Add(IdentityProofLifetime),
	}
	if !proof.ExpiresAt.Before(proof.LeafNotAfter) {
		return IdentityProof{}, ErrIdentityProofTimeWindow
	}
	canonical, err := proof.CanonicalBytes()
	if err != nil {
		return IdentityProof{}, err
	}
	digest := sha256.Sum256(canonical)
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return IdentityProof{}, ErrInvalidIdentityProof
	}
	proof.Signature = base64.RawURLEncoding.EncodeToString(signature)
	return proof, nil
}

func validateSigningIdentity(id identity.Identity) error {
	if !id.InstallationID.IsValid() || !id.NodeID.IsValid() ||
		(id.Role != domain.TopologyRolePrimary && id.Role != domain.TopologyRolePrimaryWithNodes) {
		return ErrIdentityProofIdentityNotEligible
	}
	if id.LeadershipGeneration == 0 || id.LatestKnownGeneration == 0 ||
		id.LeadershipGeneration > id.LatestKnownGeneration || id.IsStalePrimary() {
		return ErrIdentityProofGeneration
	}
	return nil
}
