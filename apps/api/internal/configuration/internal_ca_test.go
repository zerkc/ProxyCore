package configuration

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/acme"
	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
)

func newEnrollmentTLSStore(t *testing.T) *Store {
	store, _ := newEnrollmentStoreView(t)
	store.secrets = secrets.NewPgStore(store.pool, base64.StdEncoding.EncodeToString(make([]byte, 32)))
	return store
}

func seedEnrollmentTLS(t *testing.T) (*Store, EnrollmentTLSMaterial, string, string, string) {
	store := newEnrollmentTLSStore(t)
	if _, err := store.UpdateEnrollmentHostnames(t.Context(), []string{"enroll.example"}); err != nil {
		t.Fatalf("configure hostnames: %v", err)
	}
	material, err := store.EnsureEnrollmentTLSMaterial(t.Context())
	if err != nil {
		t.Fatalf("issue material: %v", err)
	}
	var caPEM, caKeyID, leafKeyID string
	if err := store.pool.QueryRow(t.Context(), `select certificate_pem, key_secret_id::text, enrollment_key_secret_id::text from internal_ca where id = $1`, internalCAID).Scan(&caPEM, &caKeyID, &leafKeyID); err != nil {
		t.Fatalf("read identity ids: %v", err)
	}
	return store, material, caPEM, caKeyID, leafKeyID
}

func TestEnsureEnrollmentTLSMaterialExposesValidatedPublicTrust(t *testing.T) {
	_, material, caPEM, caKeyID, leafKeyID := seedEnrollmentTLS(t)
	if material.PrivateKeyPEM == "" {
		t.Fatal("internal runtime material lost the leaf private key")
	}
	if material.CACertificatePEM != caPEM {
		t.Fatal("returned CA certificate does not match the validated persisted CA")
	}
	ca := parseEnrollmentCertificate(t, material.CACertificatePEM)
	if want := acme.DERHashSHA256(ca.Raw); material.CADERHashSHA256 != want {
		t.Fatalf("CA fingerprint=%q, want %q", material.CADERHashSHA256, want)
	}
	public := material.Public()
	encoded, err := json.Marshal(public)
	if err != nil {
		t.Fatalf("marshal public material: %v", err)
	}
	serialized := string(encoded)
	for _, forbidden := range []string{"PRIVATE KEY", "PrivateKeyPEM", caKeyID, leafKeyID, "ciphertext"} {
		if strings.Contains(serialized, forbidden) {
			t.Fatalf("public material serialized %q", forbidden)
		}
	}
	internalEncoded, err := json.Marshal(material)
	if err != nil {
		t.Fatalf("marshal internal material: %v", err)
	}
	if strings.Contains(string(internalEncoded), "PRIVATE KEY") || strings.Contains(string(internalEncoded), material.PrivateKeyPEM) {
		t.Fatal("internal material JSON serialized the leaf private key")
	}
	if public.CACertificatePEM != material.CACertificatePEM || public.CADERHashSHA256 != material.CADERHashSHA256 {
		t.Fatal("public projection changed validated trust material")
	}
}

func TestEnsureEnrollmentTLSMaterialReloadsExistingPublicTrust(t *testing.T) {
	firstStore, reopen := newEnrollmentStoreView(t)
	const masterKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	firstStore.secrets = secrets.NewPgStore(firstStore.pool, masterKey)
	if _, err := firstStore.UpdateEnrollmentHostnames(t.Context(), []string{"enroll.example"}); err != nil {
		t.Fatalf("configure hostnames: %v", err)
	}
	first, err := firstStore.EnsureEnrollmentTLSMaterial(t.Context())
	if err != nil {
		t.Fatalf("issue material: %v", err)
	}
	secondStore := reopen()
	secondStore.secrets = secrets.NewPgStore(secondStore.pool, masterKey)
	second, err := secondStore.EnsureEnrollmentTLSMaterial(t.Context())
	if err != nil {
		t.Fatalf("reload material: %v", err)
	}
	if first.CertificatePEM != second.CertificatePEM || first.CACertificatePEM != second.CACertificatePEM || first.CADERHashSHA256 != second.CADERHashSHA256 || !first.ExpiresAt.Equal(second.ExpiresAt) {
		t.Fatal("reloaded public enrollment trust material changed")
	}
	if second.PrivateKeyPEM == "" {
		t.Fatal("reloaded internal material lost the leaf private key")
	}
}

func TestEnsureEnrollmentTLSMaterialIssuesAndKeepsSPKI(t *testing.T) {
	store, first, caPEM, caKeyID, leafKeyID := seedEnrollmentTLS(t)
	var purpose string
	if err := store.pool.QueryRow(t.Context(), `select purpose from secrets where id = $1`, leafKeyID).Scan(&purpose); err != nil {
		t.Fatalf("read leaf secret purpose: %v", err)
	}
	if purpose != EnrollmentLeafKeyPurpose {
		t.Fatalf("leaf secret purpose=%q", purpose)
	}
	firstCert := parseEnrollmentCertificate(t, first.CertificatePEM)
	firstSPKI := enrollmentSPKI(t, firstCert)
	caKey, err := store.secrets.Get(t.Context(), caKeyID)
	if err != nil {
		t.Fatalf("read CA key: %v", err)
	}
	short, err := acme.IssueSignedByCAWithKey([]string{"enroll.example"}, 1, caPEM, caKey, first.PrivateKeyPEM)
	if err != nil {
		t.Fatalf("short leaf: %v", err)
	}
	if _, err := store.pool.Exec(t.Context(), `update internal_ca set enrollment_certificate_pem = $1 where id = $2`, short.CertificatePEM, internalCAID); err != nil {
		t.Fatalf("seed near expiry: %v", err)
	}
	renewed, err := store.EnsureEnrollmentTLSMaterial(t.Context())
	if err != nil || !bytes.Equal(firstSPKI, enrollmentSPKI(t, parseEnrollmentCertificate(t, renewed.CertificatePEM))) {
		t.Fatalf("stable near-expiry renewal: %v", err)
	}
	if _, err := store.UpdateEnrollmentHostnames(t.Context(), []string{"enroll.example", "192.0.2.10"}); err != nil {
		t.Fatalf("change SANs: %v", err)
	}
	updated, err := store.EnsureEnrollmentTLSMaterial(t.Context())
	if err != nil {
		t.Fatalf("SAN renewal: %v", err)
	}
	cert := parseEnrollmentCertificate(t, updated.CertificatePEM)
	if !bytes.Equal(firstSPKI, enrollmentSPKI(t, cert)) || len(cert.DNSNames) != 1 || cert.DNSNames[0] != "enroll.example" || len(cert.IPAddresses) != 1 || !cert.IPAddresses[0].Equal(net.ParseIP("192.0.2.10")) {
		t.Fatalf("renewed identity SAN/SPKI mismatch: %v/%v", cert.DNSNames, cert.IPAddresses)
	}
	var ca, leaf, caID, leafID string
	if err := store.pool.QueryRow(t.Context(), `select certificate_pem, enrollment_certificate_pem, key_secret_id::text, enrollment_key_secret_id::text from internal_ca where id = $1`, internalCAID).Scan(&ca, &leaf, &caID, &leafID); err != nil {
		t.Fatalf("read persisted identity: %v", err)
	}
	for _, value := range []string{ca, leaf, caID, leafID} {
		if strings.Contains(value, "PRIVATE KEY") {
			t.Fatal("private key PEM persisted in internal_ca row")
		}
	}
}

func TestEnsureEnrollmentTLSMaterialFailsClosed(t *testing.T) {
	for _, name := range []string{"missing CA key", "corrupt CA", "tampered CA signature", "missing leaf key", "deleted internal CA", "cleared enrollment leaf", "wrong key", "wrong chain", "bad EKU", "expired leaf", "malformed SAN"} {
		t.Run(name, func(t *testing.T) {
			store, first, caPEM, caKeyID, leafKeyID := seedEnrollmentTLS(t)
			var mutateErr error
			switch name {
			case "missing CA key":
				_, mutateErr = store.pool.Exec(t.Context(), `update secrets set ciphertext = 'bad' where id = $1`, caKeyID)
			case "corrupt CA":
				_, mutateErr = store.pool.Exec(t.Context(), `update internal_ca set certificate_pem = 'bad' where id = $1`, internalCAID)
			case "tampered CA signature":
				block, _ := pem.Decode([]byte(caPEM))
				der := append([]byte(nil), block.Bytes...)
				der[len(der)-1] ^= 1
				tampered := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
				_, mutateErr = store.pool.Exec(t.Context(), `update internal_ca set certificate_pem = $1 where id = $2`, tampered, internalCAID)
			case "missing leaf key":
				_, mutateErr = store.pool.Exec(t.Context(), `update secrets set ciphertext = 'bad' where id = $1`, leafKeyID)
			case "deleted internal CA":
				_, mutateErr = store.pool.Exec(t.Context(), `delete from internal_ca where id = $1`, internalCAID)
			case "cleared enrollment leaf":
				_, mutateErr = store.pool.Exec(t.Context(), `update internal_ca set enrollment_certificate_pem = null, enrollment_key_secret_id = null where id = $1`, internalCAID)
			case "wrong key":
				other, _ := acme.CreateInternalCA(3650)
				var wrongID string
				wrongID, mutateErr = store.secrets.Put(t.Context(), EnrollmentLeafKeyPurpose, other.PrivateKeyPEM)
				if mutateErr == nil {
					_, mutateErr = store.pool.Exec(t.Context(), `update internal_ca set enrollment_key_secret_id = $1 where id = $2`, wrongID, internalCAID)
				}
			case "wrong chain":
				other, _ := acme.CreateInternalCA(3650)
				var cert acme.Material
				cert, mutateErr = acme.IssueSignedByCAWithKey([]string{"enroll.example"}, 365, other.CertificatePEM, other.PrivateKeyPEM, first.PrivateKeyPEM)
				if mutateErr == nil {
					_, mutateErr = store.pool.Exec(t.Context(), `update internal_ca set enrollment_certificate_pem = $1 where id = $2`, cert.CertificatePEM, internalCAID)
				}
			case "bad EKU", "expired leaf", "malformed SAN":
				caKey, _ := store.secrets.Get(t.Context(), caKeyID)
				bad := variantCertificate(t, caPEM, caKey, first.CertificatePEM, name == "expired leaf", name == "malformed SAN")
				_, mutateErr = store.pool.Exec(t.Context(), `update internal_ca set enrollment_certificate_pem = $1 where id = $2`, bad, internalCAID)
			}
			if mutateErr != nil {
				t.Fatalf("mutate established material: %v", mutateErr)
			}
			if _, err := store.EnsureEnrollmentTLSMaterial(t.Context()); err == nil {
				t.Fatalf("accepted established %s", name)
			}
		})
	}
}

func TestEnrollmentSANMatchingRejectsMalformedAndIncompleteNames(t *testing.T) {
	if sameEnrollmentHostnames(&x509.Certificate{DNSNames: []string{"192.0.2.10"}}, []string{"192.0.2.10"}) {
		t.Fatal("accepted an IP encoded as a DNS SAN")
	}
	if sameEnrollmentHostnames(&x509.Certificate{DNSNames: []string{"enroll.example"}}, []string{"enroll.example", "192.0.2.10"}) {
		t.Fatal("accepted incomplete configured SAN coverage")
	}
}

func TestEnsureEnrollmentTLSMaterialConcurrentCallersConverge(t *testing.T) {
	store := newEnrollmentTLSStore(t)
	if _, err := store.UpdateEnrollmentHostnames(t.Context(), []string{"enroll.example"}); err != nil {
		t.Fatalf("configure hostnames: %v", err)
	}
	const callers = 16
	results := make(chan EnrollmentTLSMaterial, callers)
	errors := make(chan error, callers)
	var start, done sync.WaitGroup
	start.Add(callers)
	done.Add(callers)
	for range callers {
		go func() {
			start.Done()
			start.Wait()
			material, err := store.EnsureEnrollmentTLSMaterial(t.Context())
			if err != nil {
				errors <- err
			} else {
				results <- material
			}
			done.Done()
		}()
	}
	done.Wait()
	close(results)
	close(errors)
	for err := range errors {
		t.Fatalf("concurrent issuance: %v", err)
	}
	var first EnrollmentTLSMaterial
	for material := range results {
		if first.CertificatePEM == "" {
			first = material
		} else if material.CertificatePEM != first.CertificatePEM {
			t.Fatal("concurrent callers returned different leaf material")
		}
	}
	var persisted string
	if err := store.pool.QueryRow(t.Context(), `select enrollment_certificate_pem from internal_ca where id = $1`, internalCAID).Scan(&persisted); err != nil {
		t.Fatalf("read committed leaf: %v", err)
	}
	if persisted != first.CertificatePEM {
		t.Fatal("caller returned leaf different from committed row")
	}
}

func variantCertificate(t *testing.T, caPEM, caKeyPEM, leafPEM string, expired, malformed bool) string {
	caPair, err := tls.X509KeyPair([]byte(caPEM), []byte(caKeyPEM))
	if err != nil {
		t.Fatalf("parse CA pair: %v", err)
	}
	ca, err := x509.ParseCertificate(caPair.Certificate[0])
	if err != nil {
		t.Fatalf("parse CA: %v", err)
	}
	leaf := parseEnrollmentCertificate(t, leafPEM)
	template := *leaf
	now := time.Now().UTC()
	template.NotBefore, template.NotAfter = now.Add(-2*time.Hour), now.Add(time.Hour)
	template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	if expired {
		template.NotAfter, template.ExtKeyUsage = now.Add(-time.Minute), []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
	if malformed {
		template.DNSNames, template.IPAddresses = []string{"192.0.2.10"}, nil
	}
	signer := caPair.PrivateKey.(crypto.Signer)
	der, err := x509.CreateCertificate(rand.Reader, &template, ca, leaf.PublicKey, signer)
	if err != nil {
		t.Fatalf("create variant: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func parseEnrollmentCertificate(t *testing.T, certificatePEM string) *x509.Certificate {
	block, _ := pem.Decode([]byte(certificatePEM))
	if block == nil {
		t.Fatal("certificate PEM decode failed")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse enrollment certificate: %v", err)
	}
	return certificate
}
func enrollmentSPKI(t *testing.T, certificate *x509.Certificate) []byte {
	spki, err := x509.MarshalPKIXPublicKey(certificate.PublicKey)
	if err != nil {
		t.Fatalf("marshal SPKI: %v", err)
	}
	return spki
}
