package enrollment

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/acme"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
)

func TestIdentityProofSignerRefreshesIdentityTimeAndMaterialPerCall(t *testing.T) {
	f := newProofFixture(t)
	ctx := context.Background()
	current, loaded, now := f.id, true, f.now
	leaf, leafKey, ca, calls := f.leaf, f.leafKey, f.ca, 0
	signer, err := NewIdentityProofSigner(IdentityProofSignerOptions{
		Identity: func(context.Context) (identity.Identity, bool, error) { return current, loaded, nil },
		Material: func(context.Context) (string, string, string, error) { calls++; return leaf, leafKey, ca, nil },
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := signer.Sign(ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	current.Role = domain.TopologyRoleNode
	if _, err := signer.Sign(ctx, f.request); err == nil {
		t.Fatal("stale role was accepted")
	}
	current.Role = domain.TopologyRolePrimary
	current.LeadershipGeneration++
	current.LatestKnownGeneration = current.LeadershipGeneration
	now = now.Add(time.Minute)
	other, err := acme.IssueSignedByCA([]string{"primary.example"}, 30, f.ca, f.caKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, leafKey = other.CertificatePEM, other.PrivateKeyPEM
	second, err := signer.Sign(ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || second.LeadershipGeneration != uint64(current.LeadershipGeneration) || !second.IssuedAt.Equal(now) || second.LeafSPKISHA256 == first.LeafSPKISHA256 {
		t.Fatalf("providers were not refreshed: calls=%d first=%#v second=%#v", calls, first, second)
	}
	loaded = false
	if _, err := signer.Sign(ctx, f.request); err == nil {
		t.Fatal("unloaded identity was accepted")
	}
}

func TestIdentityProofSignerRejectsNonExactOrNonRSAKeyPEM(t *testing.T) {
	f := newProofFixture(t)
	keyBlock, _ := pem.Decode([]byte(f.leafKey))
	if keyBlock == nil {
		t.Fatal("leaf key did not decode")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	rsaKey, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		t.Fatal("fixture key is not RSA")
	}
	rsaPKCS1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey)})
	ecdsaKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecdsaDER, err := x509.MarshalPKCS8PrivateKey(ecdsaKey)
	if err != nil {
		t.Fatal(err)
	}
	_, ed25519Key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ed25519DER, err := x509.MarshalPKCS8PrivateKey(ed25519Key)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		key  string
		ok   bool
	}{
		{"canonical PKCS8 RSA", f.leafKey, true},
		{"canonical PKCS1 RSA", string(rsaPKCS1), true},
		{"leading junk", "junk" + f.leafKey, false},
		{"leading whitespace", "\n" + f.leafKey, false},
		{"trailing bytes", f.leafKey + "trailing", false},
		{"RSA DER relabeled certificate", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: keyBlock.Bytes})), false},
		{"RSA DER relabeled EC key", string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBlock.Bytes})), false},
		{"RSA DER relabeled unknown key", string(pem.EncodeToMemory(&pem.Block{Type: "NOT A KEY", Bytes: keyBlock.Bytes})), false},
		{"actual ECDSA PKCS8", string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecdsaDER})), false},
		{"actual Ed25519 PKCS8", string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ed25519DER})), false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			signer, err := NewIdentityProofSigner(IdentityProofSignerOptions{
				Identity: func(context.Context) (identity.Identity, bool, error) { return f.id, true, nil },
				Material: func(context.Context) (string, string, string, error) { return f.leaf, tt.key, f.ca, nil },
				Now:      func() time.Time { return f.now },
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = signer.Sign(context.Background(), f.request)
			if tt.ok && err != nil {
				t.Fatalf("valid RSA key rejected: %v", err)
			}
			if !tt.ok && err == nil {
				t.Fatal("invalid or non-RSA key accepted")
			}
		})
	}
}

func TestIdentityProofSignerRejectsSelfInvalidCA(t *testing.T) {
	f := newProofFixture(t)
	badCA := selfInvalidCAPEM(t, f.ca)
	signer, err := NewIdentityProofSigner(IdentityProofSignerOptions{
		Identity: func(context.Context) (identity.Identity, bool, error) { return f.id, true, nil },
		Material: func(context.Context) (string, string, string, error) { return f.leaf, f.leafKey, badCA, nil },
		Now:      func() time.Time { return f.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := signer.Sign(context.Background(), f.request); err == nil {
		t.Fatal("self-invalid CA was accepted")
	}
}

func TestIdentityProofSignerExpiryIsStrictlyBeforeLeafExpiry(t *testing.T) {
	f := newProofFixture(t)
	now := f.now.Truncate(time.Second)
	for _, test := range []struct {
		name   string
		offset time.Duration
		valid  bool
	}{
		{"equal", 0, false},
		{"before", -time.Second, false},
		{"near-after", time.Second, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			leaf := leafPEMWithExpiry(t, f.ca, f.caKey, f.leafKey, now.Add(IdentityProofLifetime+test.offset))
			signer, err := NewIdentityProofSigner(IdentityProofSignerOptions{
				Identity: func(context.Context) (identity.Identity, bool, error) { return f.id, true, nil },
				Material: func(context.Context) (string, string, string, error) { return leaf, f.leafKey, f.ca, nil },
				Now:      func() time.Time { return now },
			})
			if err != nil {
				t.Fatal(err)
			}
			proof, err := signer.Sign(context.Background(), f.request)
			if test.valid {
				if err != nil {
					t.Fatal(err)
				}
				opts := IdentityProofVerifierOptions{Request: f.request, ExpectedInstallationID: f.id.InstallationID, ExpectedNodeID: f.id.NodeID, ExpectedLeadershipGeneration: f.id.LeadershipGeneration, ExpectedLeafSPKISHA256: proof.LeafSPKISHA256, ExpectedCADERHashSHA256: proof.CADERHashSHA256, LeafCertificatePEM: leaf, CACertificatePEM: f.ca, Now: now}
				if err := VerifyIdentityProof(proof, opts); err != nil {
					t.Fatalf("near-boundary proof did not verify: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("proof was signed at or beyond leaf expiry")
			}
		})
	}
}

func selfInvalidCAPEM(t *testing.T, caPEM string) string {
	t.Helper()
	ca, err := parseCertificate(caPEM)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		TBSCertificate     asn1.RawValue
		SignatureAlgorithm asn1.RawValue
		SignatureValue     asn1.BitString
	}
	if _, err := asn1.Unmarshal(ca.Raw, &envelope); err != nil {
		t.Fatal(err)
	}
	envelope.SignatureValue.Bytes[0] ^= 1
	der, err := asn1.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func leafPEMWithExpiry(t *testing.T, caPEM, caKeyPEM, leafKeyPEM string, notAfter time.Time) string {
	t.Helper()
	ca, err := parseCertificate(caPEM)
	if err != nil {
		t.Fatal(err)
	}
	caKey, err := parseRSAKey(caKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := parseRSAKey(leafKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(77), Subject: pkix.Name{CommonName: "primary.example"}, NotBefore: notAfter.Add(-10 * time.Minute), NotAfter: notAfter, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, DNSNames: []string{"primary.example"}}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}
