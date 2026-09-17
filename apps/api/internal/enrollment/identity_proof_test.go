package enrollment

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/acme"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
)

type proofFixture struct {
	request                  IdentityProofRequest
	id                       identity.Identity
	ca, caKey, leaf, leafKey string
	now                      time.Time
	signer                   *IdentityProofSigner
	proof                    IdentityProof
}

func newProofSigner(id identity.Identity, now time.Time, leaf, leafKey, ca string) (*IdentityProofSigner, error) {
	return NewIdentityProofSigner(IdentityProofSignerOptions{
		Identity: func(context.Context) (identity.Identity, bool, error) { return id, true, nil },
		Material: func(context.Context) (string, string, string, error) { return leaf, leafKey, ca, nil },
		Now:      func() time.Time { return now },
	})
}

func newProofFixture(t *testing.T) proofFixture {
	t.Helper()
	caMaterial, err := acme.CreateInternalCA(30)
	if err != nil {
		t.Fatal(err)
	}
	leafMaterial, err := acme.IssueSignedByCA([]string{"primary.example"}, 30, caMaterial.CertificatePEM, caMaterial.PrivateKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	id := identity.Identity{InstallationID: domain.NewInstallationID(), NodeID: domain.NewNodeID(), Role: domain.TopologyRolePrimary, LeadershipGeneration: 4, LatestKnownGeneration: 4}
	request, err := NewIdentityProofRequest("https://primary.example:3443/")
	if err != nil {
		t.Fatal(err)
	}
	signer, err := newProofSigner(id, now, leafMaterial.CertificatePEM, leafMaterial.PrivateKeyPEM, caMaterial.CertificatePEM)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := signer.Sign(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	return proofFixture{request, id, caMaterial.CertificatePEM, caMaterial.PrivateKeyPEM, leafMaterial.CertificatePEM, leafMaterial.PrivateKeyPEM, now, signer, proof}
}

func (f proofFixture) options(now time.Time) IdentityProofVerifierOptions {
	return IdentityProofVerifierOptions{Request: f.request, ExpectedInstallationID: f.id.InstallationID, ExpectedNodeID: f.id.NodeID, ExpectedLeadershipGeneration: f.id.LeadershipGeneration, ExpectedLeafSPKISHA256: f.proof.LeafSPKISHA256, ExpectedCADERHashSHA256: f.proof.CADERHashSHA256, LeafCertificatePEM: f.leaf, CACertificatePEM: f.ca, Now: now}
}

func TestIdentityProofRoundTripAndCanonicalBytes(t *testing.T) {
	f := newProofFixture(t)
	if err := VerifyIdentityProof(f.proof, f.options(f.now)); err != nil {
		t.Fatalf("verify: %v", err)
	}
	first, err := f.proof.CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	second, err := CanonicalIdentityProofBytes(f.proof)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("canonical bytes are not deterministic: %v", err)
	}
	if len(first) < 100 || f.proof.Signature == "" || f.proof.LeafSPKISHA256 == "" || f.proof.CADERHashSHA256 == "" {
		t.Fatalf("incomplete proof: %#v", f.proof)
	}
}

func TestIdentityProofRejectsEveryBoundFieldTamper(t *testing.T) {
	f := newProofFixture(t)
	mutations := []struct {
		name   string
		mutate func(*IdentityProof)
	}{
		{"version", func(p *IdentityProof) { p.Version++ }}, {"algorithm", func(p *IdentityProof) { p.SignatureAlgorithm = "rsa-pss-sha256" }},
		{"attempt", func(p *IdentityProof) { p.AttemptID = uuid.NewString() }}, {"nonce", func(p *IdentityProof) { p.Nonce = strings.Repeat("A", 43) }},
		{"url", func(p *IdentityProof) { p.PrimaryURL = "https://other.example:3443/" }}, {"installation", func(p *IdentityProof) { p.InstallationID = domain.NewInstallationID().String() }},
		{"node", func(p *IdentityProof) { p.NodeID = domain.NewNodeID().String() }}, {"role", func(p *IdentityProof) { p.Role = string(domain.TopologyRolePrimaryWithNodes) }},
		{"generation", func(p *IdentityProof) { p.LeadershipGeneration++ }}, {"spki", func(p *IdentityProof) { p.LeafSPKISHA256 = strings.Repeat("0", 64) }},
		{"ca", func(p *IdentityProof) { p.CADERHashSHA256 = strings.Repeat("0", 64) }}, {"leaf not before", func(p *IdentityProof) { p.LeafNotBefore = p.LeafNotBefore.Add(time.Second) }},
		{"leaf not after", func(p *IdentityProof) { p.LeafNotAfter = p.LeafNotAfter.Add(-time.Second) }}, {"issued", func(p *IdentityProof) { p.IssuedAt = p.IssuedAt.Add(time.Second) }},
		{"expires", func(p *IdentityProof) { p.ExpiresAt = p.ExpiresAt.Add(-time.Second) }}, {"signature", func(p *IdentityProof) { p.Signature = strings.Repeat("A", len(p.Signature)) }},
	}
	for _, tt := range mutations {
		t.Run(tt.name, func(t *testing.T) {
			proof := f.proof
			tt.mutate(&proof)
			if VerifyIdentityProof(proof, f.options(f.now)) == nil {
				t.Fatal("tampered proof verified")
			}
		})
	}
}

func TestIdentityProofRejectsWrongSignerCAAndSPKI(t *testing.T) {
	f := newProofFixture(t)
	other, err := acme.IssueSignedByCA([]string{"primary.example"}, 30, f.ca, f.caKey)
	if err != nil {
		t.Fatal(err)
	}
	otherSigner, err := newProofSigner(f.id, f.now, other.CertificatePEM, other.PrivateKeyPEM, f.ca)
	if err != nil {
		t.Fatal(err)
	}
	wrongSignerProof, err := otherSigner.Sign(context.Background(), f.request)
	if err != nil {
		t.Fatal(err)
	}
	if VerifyIdentityProof(wrongSignerProof, f.options(f.now)) == nil {
		t.Fatal("wrong signer verified")
	}
	ca2, err := acme.CreateInternalCA(30)
	if err != nil {
		t.Fatal(err)
	}
	badCA := f.options(f.now)
	badCA.CACertificatePEM = ca2.CertificatePEM
	badCA.ExpectedCADERHashSHA256 = ""
	if VerifyIdentityProof(f.proof, badCA) == nil {
		t.Fatal("wrong CA verified")
	}
	badSPKI := f.options(f.now)
	badSPKI.LeafCertificatePEM = other.CertificatePEM
	badSPKI.ExpectedLeafSPKISHA256 = ""
	if VerifyIdentityProof(f.proof, badSPKI) == nil {
		t.Fatal("wrong SPKI verified")
	}
}

func TestIdentityProofRejectsExpiryAndNotYetValid(t *testing.T) {
	f := newProofFixture(t)
	expired := f.options(f.proof.ExpiresAt)
	if VerifyIdentityProof(f.proof, expired) == nil {
		t.Fatal("expired proof verified")
	}
	future := f.options(f.proof.IssuedAt.Add(-time.Nanosecond))
	if VerifyIdentityProof(f.proof, future) == nil {
		t.Fatal("not-yet-valid proof verified")
	}
	tooHigh := f.options(f.now)
	tooHigh.ExpectedLeadershipGeneration = f.id.LeadershipGeneration + 1
	if VerifyIdentityProof(f.proof, tooHigh) == nil {
		t.Fatal("regressing generation verified")
	}
}

func TestIdentityProofSigningRoleGenerationAndLoadBoundaries(t *testing.T) {
	f := newProofFixture(t)
	for _, role := range []domain.TopologyRole{domain.TopologyRoleStandalone, domain.TopologyRoleNode, domain.TopologyRoleStalePrimary} {
		id := f.id
		id.Role = role
		signer, err := newProofSigner(id, f.now, f.leaf, f.leafKey, f.ca)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := signer.Sign(context.Background(), f.request); err == nil {
			t.Fatalf("role %q signed", role)
		}
	}
	for _, id := range []identity.Identity{{InstallationID: f.id.InstallationID, NodeID: f.id.NodeID, Role: domain.TopologyRolePrimary, LeadershipGeneration: 0, LatestKnownGeneration: 0}, {InstallationID: f.id.InstallationID, NodeID: f.id.NodeID, Role: domain.TopologyRolePrimary, LeadershipGeneration: 2, LatestKnownGeneration: 1}, {InstallationID: f.id.InstallationID, NodeID: f.id.NodeID, Role: domain.TopologyRolePrimary, LeadershipGeneration: 1, LatestKnownGeneration: 2}} {
		signer, err := newProofSigner(id, f.now, f.leaf, f.leafKey, f.ca)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := signer.Sign(context.Background(), f.request); err == nil {
			t.Fatal("invalid generation signed")
		}
	}
	unloaded, err := NewIdentityProofSigner(IdentityProofSignerOptions{
		Identity: func(context.Context) (identity.Identity, bool, error) { return f.id, false, nil },
		Material: func(context.Context) (string, string, string, error) { return f.leaf, f.leafKey, f.ca, nil },
		Now:      func() time.Time { return f.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = unloaded.Sign(context.Background(), f.request); !errors.Is(err, ErrIdentityProofIdentityNotLoaded) {
		t.Fatalf("unloaded error = %v", err)
	}
	id := f.id
	id.Role = domain.TopologyRolePrimaryWithNodes
	signer, err := newProofSigner(id, f.now, f.leaf, f.leafKey, f.ca)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = signer.Sign(context.Background(), f.request); err != nil {
		t.Fatalf("primary-with-nodes sign: %v", err)
	}
}

func TestIdentityProofRejectsMalformedAttemptAndNonce(t *testing.T) {
	f := newProofFixture(t)
	for _, request := range []IdentityProofRequest{{AttemptID: "not-a-uuid", Nonce: f.request.Nonce, PrimaryURL: f.request.PrimaryURL}, {AttemptID: f.request.AttemptID, Nonce: f.request.Nonce + "=", PrimaryURL: f.request.PrimaryURL}, {AttemptID: f.request.AttemptID, Nonce: "AA", PrimaryURL: f.request.PrimaryURL}} {
		if request.Validate() == nil {
			t.Fatal("malformed request accepted")
		}
	}
	if _, err := NewIdentityProofRequest("HTTPS://primary.example:3443/"); err == nil {
		t.Fatal("noncanonical URL accepted")
	}
}

func TestIdentityProofRejectsUnsupportedKeyAndAlgorithm(t *testing.T) {
	f := newProofFixture(t)
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(ec)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	signer, err := NewIdentityProofSigner(IdentityProofSignerOptions{
		Identity: func(context.Context) (identity.Identity, bool, error) { return f.id, true, nil },
		Material: func(context.Context) (string, string, string, error) { return f.leaf, string(keyPEM), f.ca, nil },
		Now:      func() time.Time { return f.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = signer.Sign(context.Background(), f.request); !errors.Is(err, ErrUnsupportedIdentityProofKey) {
		t.Fatalf("unsupported key error = %v", err)
	}
	proof := f.proof
	proof.SignatureAlgorithm = "ecdsa-sha256"
	if VerifyIdentityProof(proof, f.options(f.now)) == nil {
		t.Fatal("unsupported algorithm verified")
	}
}

func TestIdentityProofSerializationAndErrorsAreRedacted(t *testing.T) {
	f := newProofFixture(t)
	encoded, err := json.Marshal(struct {
		Request IdentityProofRequest
		Proof   IdentityProof
	}{f.request, f.proof})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"private key", "secret", "token", "credential", "kek"} {
		if strings.Contains(strings.ToLower(string(encoded)), forbidden) {
			t.Fatalf("serialized proof contains %q", forbidden)
		}
	}
	signer, err := NewIdentityProofSigner(IdentityProofSignerOptions{
		Identity: func(context.Context) (identity.Identity, bool, error) { return f.id, true, nil },
		Material: func(context.Context) (string, string, string, error) {
			return f.leaf, "leak-private-key-token-credential", f.ca, nil
		},
		Now: func() time.Time { return f.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = signer.Sign(context.Background(), f.request)
	if strings.Contains(err.Error(), "leak-private-key-token-credential") {
		t.Fatal("error leaked key input")
	}
}
