package configuration

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"math/big"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/acme"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// Error strings and field values below were observed on postgres:17-alpine
// against Store.IssueCertificate / RenewSelfSignedCertificateByID. Tests assert
// by structure, booleans and fingerprints only: no key, PEM or ciphertext text
// ever reaches a failure message.

const (
	issueOwnerA = "aaaaaaaa-0000-4000-8000-0000000000a1"
	issueOwnerB = "aaaaaaaa-0000-4000-8000-0000000000a2"
	issueOwnerC = "aaaaaaaa-0000-4000-8000-0000000000a3"
	issueOwnerD = "aaaaaaaa-0000-4000-8000-0000000000a4"
	keyPurpose  = "certificate-private-key"
)

var issueUUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type issueFixture struct {
	*certFixture
	issuer *SQLiteCertificateIssueStore
}

func newIssueFixture(t *testing.T) *issueFixture {
	t.Helper()
	f := newCertFixture(t)
	return &issueFixture{certFixture: f, issuer: NewSQLiteCertificateIssueStore(f.db, settingsWriter(f.db), f.secrets)}
}

func (f *issueFixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *issueFixture) rows(t *testing.T) (certs, keySecrets, allSecrets int) {
	t.Helper()
	return f.count(t, `select count(*) from certificates`),
		f.count(t, `select count(*) from secrets where purpose = ?`, keyPurpose),
		f.count(t, `select count(*) from secrets`)
}

func (f *issueFixture) assertNoCertificateState(t *testing.T) {
	t.Helper()
	if certs, keys, _ := f.rows(t); certs != 0 || keys != 0 {
		t.Fatalf("failed issue left state: certificates=%d key secrets=%d", certs, keys)
	}
}

func (f *issueFixture) issueSelfSigned(t *testing.T, hostnames ...string) domain.CertificateStatus {
	t.Helper()
	cert, err := f.issuer.IssueCertificate(context.Background(), CertificateIssueInput{Hostnames: hostnames, Issuer: "self-signed", Challenge: "none"}, "")
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func (f *issueFixture) seedOwner(t *testing.T, id string, role string, active int, created string) {
	t.Helper()
	settingsExec(t, f.db, `insert into users (id,username,password_hash,role,active,created_at,updated_at) values (?,?,'x',?,?,?,?)`,
		id, "user-"+id[len(id)-2:], role, active, created, created)
}

type pemPair struct{ cert, key string }

func newPEMPair(t *testing.T, names []string, notAfter time.Time, pkcs1, mismatch bool) pemPair {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: names[0]},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter, DNSNames: names}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	signer := key
	if mismatch {
		if signer, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			t.Fatal(err)
		}
	}
	block := &pem.Block{Type: "PRIVATE KEY"}
	if pkcs1 {
		block = &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(signer)}
	} else if block.Bytes, err = x509.MarshalPKCS8PrivateKey(signer); err != nil {
		t.Fatal(err)
	}
	return pemPair{cert: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), key: string(pem.EncodeToMemory(block))}
}

func (p pemPair) input(hostnames ...string) CertificateIssueInput {
	return CertificateIssueInput{Hostnames: hostnames, Issuer: "uploaded", Challenge: "none", CertificatePEM: p.cert, PrivateKeyPEM: p.key}
}

// publicKeyMatches reports whether the stored secret parses and pairs with cert.
func publicKeyMatches(t *testing.T, f *issueFixture, secretID, certPEM string) bool {
	t.Helper()
	keyPEM, err := f.secrets.Get(context.Background(), secretID)
	if err != nil {
		t.Fatal(err)
	}
	key, err := acme.ParseRSAPrivateKeyPEM(keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	pub, ok := parseCertPEM(t, certPEM).PublicKey.(*rsa.PublicKey)
	return ok && pub.Equal(&key.PublicKey)
}

// failCertSecrets fails only the certificate key purpose, so the internal CA
// still bootstraps and the failure lands inside the certificate Write.
type failCertSecrets struct {
	*secrets.SQLiteStore
	err error
}

func (s failCertSecrets) PutInWrite(v sqlitedb.WriteValue, purpose, plaintext string) (string, error) {
	if purpose == keyPurpose {
		return "", s.err
	}
	return s.SQLiteStore.PutInWrite(v, purpose, plaintext)
}

func TestSQLiteCertificateIssueSelfSignedRowMatchesPG(t *testing.T) {
	f := newIssueFixture(t)
	hostnames := []string{"Example.TEST", "example.test", " *.Foo.Test ", "192.0.2.5", "[2001:DB8::1]"}
	cert := f.issueSelfSigned(t, hostnames...)

	if !issueUUIDPattern.MatchString(cert.ID) || !issueUUIDPattern.MatchString(cert.SecretID) {
		t.Fatal("certificate and secret ids must be lowercase uuids")
	}
	if cert.Issuer != "self-signed" || cert.Challenge != "none" || cert.Environment != "local" || cert.Status != "active" || cert.FailureReason != "" {
		t.Fatalf("unexpected row fields: issuer=%s challenge=%s env=%s status=%s", cert.Issuer, cert.Challenge, cert.Environment, cert.Status)
	}
	// PG stores the hostnames exactly as given: no trim, dedup or lowercase.
	if !reflect.DeepEqual(cert.Hostnames, hostnames) {
		t.Fatalf("hostnames were rewritten: %q", cert.Hostnames)
	}
	if cert.ExpiresAt == nil || cert.RenewAfter == nil || !within(*cert.ExpiresAt, time.Now().Add(365*24*time.Hour), time.Minute) {
		t.Fatal("expiry is not about 365 days out")
	}
	if !cert.RenewAfter.Equal(cert.ExpiresAt.Add(-30 * 24 * time.Hour)) {
		t.Fatal("renew_after must be expires_at minus 30 days")
	}
	leaf := parseCertPEM(t, cert.CertificatePEM)
	if !leaf.NotAfter.Equal(cert.ExpiresAt.Truncate(time.Second)) {
		t.Fatal("expires_at differs from the leaf NotAfter")
	}

	stored, err := f.issuer.GetCertificate(context.Background(), cert.ID)
	if err != nil || !reflect.DeepEqual(stored, cert) {
		t.Fatalf("returned value differs from the stored row: %v", err)
	}
	var purpose, createdAt, updatedAt string
	if err := f.db.QueryRow(`select s.purpose, c.created_at, c.updated_at from certificates c join secrets s on s.id = c.key_secret_id where c.id = ?`, cert.ID).
		Scan(&purpose, &createdAt, &updatedAt); err != nil {
		t.Fatal(err)
	}
	stamp := *certKey(t, 123, 456)
	if purpose != keyPurpose || createdAt != stamp || updatedAt != stamp {
		t.Fatal("secret purpose or writer clock differs")
	}
	if certs, keys, all := f.rows(t); certs != 1 || keys != 1 || all != 2 {
		t.Fatalf("expected 1 certificate, 1 leaf key and the CA key; got %d/%d/%d", certs, keys, all)
	}
}

func TestSQLiteCertificateIssueSelfSignedKeyIsEncryptedAndChainsToCA(t *testing.T) {
	f := newIssueFixture(t)
	ctx := context.Background()
	cert := f.issueSelfSigned(t, "svc.example.test", "192.0.2.9")

	var ciphertext string
	if err := f.db.QueryRow(`select ciphertext from secrets where id = ?`, cert.SecretID).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ciphertext, "PRIVATE KEY") || strings.Contains(ciphertext, "BEGIN") {
		t.Fatal("private key is stored unencrypted")
	}
	if !publicKeyMatches(t, f, cert.SecretID, cert.CertificatePEM) {
		t.Fatal("decrypted key does not pair with the certificate")
	}

	caPEM, err := f.issuer.EnsureInternalCACertificatePEM(ctx)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(parseCertPEM(t, caPEM))
	leaf := parseCertPEM(t, cert.CertificatePEM)
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "svc.example.test", Roots: roots}); err != nil {
		t.Fatalf("leaf does not verify against the internal CA: %v", err)
	}
	if len(leaf.IPAddresses) != 1 || leaf.IPAddresses[0].String() != "192.0.2.9" {
		t.Fatal("IP SAN missing")
	}
	if f.count(t, `select count(*) from internal_ca`) != 1 {
		t.Fatal("expected exactly one internal CA")
	}
}

func TestSQLiteCertificateIssueEnvironmentAndIdAreHonored(t *testing.T) {
	f := newIssueFixture(t)
	cert, err := f.issuer.IssueCertificate(context.Background(),
		CertificateIssueInput{Hostnames: []string{"a.test"}, Issuer: "self-signed", Challenge: "none", Environment: "production"}, "ignored-actor")
	if err != nil || cert.Environment != "production" {
		t.Fatalf("explicit environment: %v %q", err, cert.Environment)
	}
	other := f.issueSelfSigned(t, "a.test")
	if other.ID == cert.ID || other.SecretID == cert.SecretID {
		t.Fatal("repeated issues must produce distinct ids")
	}
}

func TestSQLiteCertificateIssueSelfSignedValidationMatchesPG(t *testing.T) {
	long := strings.Repeat("a", 70) + ".test"
	cases := []struct {
		name  string
		input CertificateIssueInput
		want  string
	}{
		{"challenge http-01", CertificateIssueInput{Hostnames: []string{"a.test"}, Issuer: "self-signed", Challenge: "http-01"}, "Self-signed issuance requires challenge none and a master key"},
		{"challenge empty", CertificateIssueInput{Hostnames: []string{"a.test"}, Issuer: "self-signed"}, "Self-signed issuance requires challenge none and a master key"},
		{"empty hostnames", CertificateIssueInput{Hostnames: []string{}, Issuer: "self-signed", Challenge: "none"}, "At least one certificate hostname is required"},
		{"nil hostnames", CertificateIssueInput{Issuer: "self-signed", Challenge: "none"}, "At least one certificate hostname is required"},
		{"bad hostname", CertificateIssueInput{Hostnames: []string{"not a host!"}, Issuer: "self-signed", Challenge: "none"}, "Invalid DNS label: not a host!"},
		{"blank hostname", CertificateIssueInput{Hostnames: []string{"  "}, Issuer: "self-signed", Challenge: "none"}, "DNS name cannot be empty"},
		{"long label", CertificateIssueInput{Hostnames: []string{long}, Issuer: "self-signed", Challenge: "none"}, "Invalid DNS label: " + strings.Repeat("a", 70)},
		{"uploaded challenge", CertificateIssueInput{Hostnames: []string{"a.test"}, Issuer: "uploaded", Challenge: "http-01"}, "Uploaded certificates require challenge none and a master key"},
		{"uploaded without pem", CertificateIssueInput{Hostnames: []string{"a.test"}, Issuer: "uploaded", Challenge: "none"}, "Uploaded certificate and private key are required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newIssueFixture(t)
			got, err := f.issuer.IssueCertificate(context.Background(), tc.input, "")
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("error = %v, want prefix %q", err, tc.want)
			}
			if !reflect.DeepEqual(got, domain.CertificateStatus{}) {
				t.Fatal("failure returned a non-zero certificate")
			}
			f.assertNoCertificateState(t)
		})
	}
}

func TestSQLiteCertificateIssueSelfSignedAcceptsWildcardAndIPOnly(t *testing.T) {
	f := newIssueFixture(t)
	for _, name := range []string{"*", "192.0.2.7"} {
		if cert := f.issueSelfSigned(t, name); cert.Status != "active" || cert.Hostnames[0] != name {
			t.Fatalf("hostname %q rejected", name)
		}
	}
}

func TestSQLiteCertificateIssueUploadedStoresValidatedPair(t *testing.T) {
	f := newIssueFixture(t)
	ctx := context.Background()
	notAfter := time.Now().Add(100 * 24 * time.Hour)
	pair := newPEMPair(t, []string{"up.example.test", "*.wild.test"}, notAfter, false, false)
	hostnames := []string{"Up.Example.test", "x.wild.test", "up.example.test"}
	cert, err := f.issuer.IssueCertificate(ctx, pair.input(hostnames...), "")
	if err != nil {
		t.Fatal(err)
	}
	if cert.Issuer != "uploaded" || cert.Challenge != "none" || cert.Environment != "local" || cert.Status != "active" {
		t.Fatalf("unexpected fields: %s %s %s %s", cert.Issuer, cert.Challenge, cert.Environment, cert.Status)
	}
	if cert.RenewAfter != nil || cert.ExpiresAt == nil || !cert.ExpiresAt.Equal(notAfter.Truncate(time.Second)) {
		t.Fatal("uploaded certificates have no renew_after and expire with the leaf")
	}
	if !reflect.DeepEqual(cert.Hostnames, hostnames) || cert.CertificatePEM != pair.cert {
		t.Fatal("hostnames and PEM must be stored as given")
	}
	stored, err := f.issuer.GetCertificate(ctx, cert.ID)
	if err != nil || !reflect.DeepEqual(stored, cert) {
		t.Fatalf("returned value differs from the stored row: %v", err)
	}
	keyPEM, err := f.secrets.Get(ctx, cert.SecretID)
	if err != nil || keyPEM != pair.key {
		t.Fatal("stored key differs from the uploaded key")
	}
	var purpose, ciphertext string
	if err := f.db.QueryRow(`select purpose, ciphertext from secrets where id = ?`, cert.SecretID).Scan(&purpose, &ciphertext); err != nil {
		t.Fatal(err)
	}
	if purpose != keyPurpose || strings.Contains(ciphertext, "PRIVATE KEY") {
		t.Fatal("uploaded key secret has the wrong purpose or is unencrypted")
	}
	// Uploads never need the internal CA.
	if certs, _, all := f.rows(t); certs != 1 || all != 1 || f.count(t, `select count(*) from internal_ca`) != 0 {
		t.Fatal("uploaded issue must not create a CA")
	}
}

func TestSQLiteCertificateIssueUploadedKeepsPKCS1KeyVerbatim(t *testing.T) {
	f := newIssueFixture(t)
	pair := newPEMPair(t, []string{"p1.test"}, time.Now().Add(48*time.Hour), true, false)
	cert, err := f.issuer.IssueCertificate(context.Background(), pair.input("p1.test"), "")
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := f.secrets.Get(context.Background(), cert.SecretID)
	if err != nil || keyPEM != pair.key || strings.Contains(keyPEM, "BEGIN PRIVATE KEY") {
		t.Fatal("PKCS#1 key must be stored exactly as uploaded")
	}
}

func TestSQLiteCertificateIssueUploadedValidationMatchesPG(t *testing.T) {
	good := newPEMPair(t, []string{"ok.test"}, time.Now().Add(time.Hour), false, false)
	expired := newPEMPair(t, []string{"e.test"}, time.Now().Add(-time.Hour), false, false)
	mismatch := newPEMPair(t, []string{"m.test"}, time.Now().Add(time.Hour), false, true)
	cases := []struct {
		name  string
		input CertificateIssueInput
		want  string
	}{
		{"expired", expired.input("e.test"), "Certificate is already expired"},
		{"key mismatch", mismatch.input("m.test"), "Certificate and private key do not match"},
		{"garbage cert body", CertificateIssueInput{Hostnames: []string{"ok.test"}, Issuer: "uploaded", Challenge: "none", CertificatePEM: "BEGIN CERTIFICATE garbage", PrivateKeyPEM: good.key}, "Certificate PEM is invalid"},
		{"garbage cert block", CertificateIssueInput{Hostnames: []string{"ok.test"}, Issuer: "uploaded", Challenge: "none", CertificatePEM: "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----", PrivateKeyPEM: good.key}, "Certificate PEM is invalid"},
		{"cert without header", CertificateIssueInput{Hostnames: []string{"ok.test"}, Issuer: "uploaded", Challenge: "none", CertificatePEM: "hello", PrivateKeyPEM: good.key}, "Certificate PEM is required"},
		{"key without header", CertificateIssueInput{Hostnames: []string{"ok.test"}, Issuer: "uploaded", Challenge: "none", CertificatePEM: good.cert, PrivateKeyPEM: "hello"}, "Private key PEM is required"},
		{"garbage key block", CertificateIssueInput{Hostnames: []string{"ok.test"}, Issuer: "uploaded", Challenge: "none", CertificatePEM: good.cert, PrivateKeyPEM: "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----"}, "Private key PEM is required"},
		{"san miss", good.input("other.test"), "Certificate SANs do not cover every requested hostname"},
		{"empty cert", CertificateIssueInput{Hostnames: []string{"ok.test"}, Issuer: "uploaded", Challenge: "none", PrivateKeyPEM: good.key}, "Uploaded certificate and private key are required"},
		{"empty key", CertificateIssueInput{Hostnames: []string{"ok.test"}, Issuer: "uploaded", Challenge: "none", CertificatePEM: good.cert}, "Uploaded certificate and private key are required"},
		{"empty hostnames", good.input(), "At least one certificate hostname is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newIssueFixture(t)
			got, err := f.issuer.IssueCertificate(context.Background(), tc.input, "")
			if err == nil || err.Error() != tc.want {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if !reflect.DeepEqual(got, domain.CertificateStatus{}) {
				t.Fatal("failure returned a non-zero certificate")
			}
			if certs, _, all := f.rows(t); certs != 0 || all != 0 {
				t.Fatalf("failed upload left state: certificates=%d secrets=%d", certs, all)
			}
		})
	}
}

func TestSQLiteCertificateIssueRequiresMasterKey(t *testing.T) {
	var typedNil *secrets.SQLiteStore
	for name, secretStore := range map[string]sqliteInternalCASecrets{"nil interface": nil, "typed nil": typedNil} {
		t.Run(name, func(t *testing.T) {
			f := newIssueFixture(t)
			issuer := NewSQLiteCertificateIssueStore(f.db, settingsWriter(f.db), secretStore)
			pair := newPEMPair(t, []string{"a.test"}, time.Now().Add(time.Hour), false, false)
			for issuerName, want := range map[string]string{
				"self-signed": "Self-signed issuance requires challenge none and a master key",
				"uploaded":    "Uploaded certificates require challenge none and a master key",
			} {
				input := pair.input("a.test")
				input.Issuer = issuerName
				if _, err := issuer.IssueCertificate(context.Background(), input, ""); err == nil || err.Error() != want {
					t.Fatalf("%s: error = %v", issuerName, err)
				}
			}
			if certs, _, all := f.rows(t); certs != 0 || all != 0 {
				t.Fatal("no master key must write nothing")
			}
		})
	}
}

func TestSQLiteCertificateIssueRejectsMalformedMasterKeyWithoutState(t *testing.T) {
	f := newIssueFixture(t)
	bad := secrets.NewSQLiteStore(f.db, "not-base64!!")
	issuer := NewSQLiteCertificateIssueStore(f.db, settingsWriter(f.db), bad)
	_, err := issuer.IssueCertificate(context.Background(), CertificateIssueInput{Hostnames: []string{"a.test"}, Issuer: "self-signed", Challenge: "none"}, "")
	if err == nil || err.Error() != "PROXYCORE_MASTER_KEY_BASE64 must decode to 32 bytes" {
		t.Fatalf("error = %v", err)
	}
	if certs, _, all := f.rows(t); certs != 0 || all != 0 {
		t.Fatal("malformed master key must write nothing")
	}
}

func TestSQLiteCertificateIssueLetsEncryptIsNotImplementedYet(t *testing.T) {
	f := newIssueFixture(t)
	for _, issuer := range []string{"letsencrypt", "", "bogus"} {
		for _, challenge := range []string{"none", "http-01", "dns-01"} {
			got, err := f.issuer.IssueCertificate(context.Background(), CertificateIssueInput{Hostnames: []string{"a.test"}, Issuer: issuer, Challenge: challenge}, "actor")
			if !errors.Is(err, ErrSQLiteLetsEncryptNotImplemented) || !reflect.DeepEqual(got, domain.CertificateStatus{}) {
				t.Fatalf("issuer %q challenge %q: %v", issuer, challenge, err)
			}
		}
	}
	// 56c writes no failed row for Let's Encrypt either: that is 56d.
	if certs, _, all := f.rows(t); certs != 0 || all != 0 {
		t.Fatal("rejected issuance must write nothing")
	}
}

func TestSQLiteCertificateIssueIsAtomicWhenTheRowFails(t *testing.T) {
	// A NUL byte in environment violates a CHECK after the key secret was inserted.
	pair := newPEMPair(t, []string{"a.test"}, time.Now().Add(time.Hour), false, false)
	for name, input := range map[string]CertificateIssueInput{
		"self-signed": {Hostnames: []string{"a.test"}, Issuer: "self-signed", Challenge: "none", Environment: "a\x00b"},
		"uploaded":    func() CertificateIssueInput { i := pair.input("a.test"); i.Environment = "a\x00b"; return i }(),
	} {
		t.Run(name, func(t *testing.T) {
			f := newIssueFixture(t)
			if _, err := f.issuer.IssueCertificate(context.Background(), input, ""); err == nil {
				t.Fatal("expected a CHECK failure")
			}
			f.assertNoCertificateState(t)
		})
	}
}

func TestSQLiteCertificateIssueIsAtomicWhenTheSecretFails(t *testing.T) {
	boom := errors.New("injected secret failure")
	pair := newPEMPair(t, []string{"a.test"}, time.Now().Add(time.Hour), false, false)
	for name, input := range map[string]CertificateIssueInput{
		"self-signed": {Hostnames: []string{"a.test"}, Issuer: "self-signed", Challenge: "none"},
		"uploaded":    pair.input("a.test"),
	} {
		t.Run(name, func(t *testing.T) {
			f := newIssueFixture(t)
			issuer := NewSQLiteCertificateIssueStore(f.db, settingsWriter(f.db), failCertSecrets{SQLiteStore: f.secrets, err: boom})
			if _, err := issuer.IssueCertificate(context.Background(), input, ""); !errors.Is(err, boom) {
				t.Fatalf("error = %v", err)
			}
			f.assertNoCertificateState(t)
		})
	}
}

func TestSQLiteCertificateIssueConcurrentCallsAreIndependent(t *testing.T) {
	f := newIssueFixture(t)
	const n = 8
	results := make([]domain.CertificateStatus, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = f.issuer.IssueCertificate(context.Background(),
				CertificateIssueInput{Hostnames: []string{"c.example.test"}, Issuer: "self-signed", Challenge: "none"}, "")
		}()
	}
	wg.Wait()
	ids, secretIDs := map[string]bool{}, map[string]bool{}
	for i := range n {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		ids[results[i].ID], secretIDs[results[i].SecretID] = true, true
	}
	if len(ids) != n || len(secretIDs) != n {
		t.Fatal("concurrent issues collided")
	}
	if certs, keys, all := f.rows(t); certs != n || keys != n || all != n+1 || f.count(t, `select count(*) from internal_ca`) != 1 {
		t.Fatalf("rows: certificates=%d keys=%d secrets=%d", certs, keys, all)
	}
}

func TestSQLiteCertificateIssueCanceledContextAndClosedDatabase(t *testing.T) {
	f := newIssueFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.issuer.IssueCertificate(ctx, CertificateIssueInput{Hostnames: []string{"a.test"}, Issuer: "self-signed", Challenge: "none"}, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
	pair := newPEMPair(t, []string{"a.test"}, time.Now().Add(time.Hour), false, false)
	if _, err := f.issuer.IssueCertificate(ctx, pair.input("a.test"), ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled upload: %v", err)
	}
	f.assertNoCertificateState(t)

	cert := f.issueSelfSigned(t, "r.test")
	if _, err := f.issuer.RenewSelfSignedCertificateByID(ctx, cert.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled renew: %v", err)
	}
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.issuer.IssueCertificate(context.Background(), pair.input("a.test"), ""); err == nil {
		t.Fatal("closed database must fail")
	}
	if _, err := f.issuer.RenewSelfSignedCertificateByID(context.Background(), cert.ID); err == nil {
		t.Fatal("closed database must fail renewal")
	}
}

func TestSQLiteCertificateRenewByIDSuccessMatchesPG(t *testing.T) {
	f := newIssueFixture(t)
	ctx := context.Background()
	original := f.issueSelfSigned(t, "r.example.test", "192.0.2.4")
	settingsExec(t, f.db, `update certificates set failure_reason = 'boom' where id = ?`, original.ID)
	// Backdate so the renewal visibly moves the expiry.
	old := time.Now().Add(-5 * 24 * time.Hour)
	settingsExec(t, f.db, `update certificates set expires_at = ?, renew_after = ? where id = ?`,
		*certKey(t, old.Unix(), 0), *certKey(t, old.Unix(), 0), original.ID)

	result, err := f.issuer.RenewSelfSignedCertificateByID(ctx, original.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Applied || result.JobID != "" {
		t.Fatal("no owner means no apply job and no error")
	}
	got := result.Certificate
	if got.ID != original.ID || got.Status != "active" || got.FailureReason != "" || got.SecretID == original.SecretID || got.CertificatePEM == original.CertificatePEM {
		t.Fatal("renewed material was not swapped in")
	}
	if !reflect.DeepEqual(got.Hostnames, original.Hostnames) || got.Issuer != "self-signed" || got.Challenge != "none" || got.Environment != "local" {
		t.Fatal("renewal changed immutable fields")
	}
	if !within(*got.ExpiresAt, time.Now().Add(365*24*time.Hour), time.Minute) || !got.RenewAfter.Equal(got.ExpiresAt.Add(-30*24*time.Hour)) {
		t.Fatal("expiry or renew_after wrong")
	}
	stored, err := f.issuer.GetCertificate(ctx, original.ID)
	if err != nil || !reflect.DeepEqual(stored, got) {
		t.Fatalf("result differs from the stored row: %v", err)
	}
	if !publicKeyMatches(t, f, got.SecretID, got.CertificatePEM) {
		t.Fatal("new key does not pair with the new certificate")
	}
	// Like PG, the previous key secret stays behind as an orphan.
	if f.count(t, `select count(*) from secrets where id = ?`, original.SecretID) != 1 {
		t.Fatal("old key secret must be left in place")
	}
	if certs, keys, _ := f.rows(t); certs != 1 || keys != 2 {
		t.Fatalf("certificates=%d key secrets=%d", certs, keys)
	}
	if f.count(t, `select count(*) from apply_jobs`) != 0 {
		t.Fatal("unexpected apply job")
	}
}

func TestSQLiteCertificateRenewByIDLookupErrorsMatchPG(t *testing.T) {
	f := newIssueFixture(t)
	ctx := context.Background()
	cert := f.issueSelfSigned(t, "a.test")
	if _, err := f.issuer.RenewSelfSignedCertificateByID(ctx, "00000000-0000-4000-8000-000000000000"); !errors.Is(err, ErrCertificateNotFound) {
		t.Fatalf("not found: %v", err)
	}
	if _, err := f.issuer.RenewSelfSignedCertificateByID(ctx, "garbage"); err == nil || err.Error() != `ERROR: invalid input syntax for type uuid: "garbage" (SQLSTATE 22P02)` {
		t.Fatalf("garbage id: %v", err)
	}
	for _, alias := range []string{strings.ToUpper(cert.ID), "{" + cert.ID + "}", strings.ReplaceAll(cert.ID, "-", "")} {
		result, err := f.issuer.RenewSelfSignedCertificateByID(ctx, alias)
		if err != nil || result.Certificate.ID != cert.ID {
			t.Fatalf("alias %q: %v", alias, err)
		}
	}
}

func TestSQLiteCertificateRenewGuardsMatchPG(t *testing.T) {
	f := newIssueFixture(t)
	ctx := context.Background()
	pair := newPEMPair(t, []string{"u.test"}, time.Now().Add(time.Hour), false, false)
	uploaded, err := f.issuer.IssueCertificate(ctx, pair.input("u.test"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.issuer.RenewSelfSignedCertificateByID(ctx, uploaded.ID); err == nil || err.Error() != "self-signed renewal requires issuer self-signed" {
		t.Fatalf("uploaded: %v", err)
	}
	ss := f.issueSelfSigned(t, "s.test")
	settingsExec(t, f.db, `update certificates set status = 'failed' where id = ?`, ss.ID)
	if _, err := f.issuer.RenewSelfSignedCertificateByID(ctx, ss.ID); err == nil || err.Error() != "certificate "+ss.ID+" is not active" {
		t.Fatalf("inactive: %v", err)
	}
	settingsExec(t, f.db, `update certificates set status = 'active', challenge = 'http-01' where id = ?`, ss.ID)
	if _, err := f.issuer.RenewSelfSignedCertificateByID(ctx, ss.ID); err == nil || err.Error() != "certificate "+ss.ID+` has unsupported challenge "http-01"` {
		t.Fatalf("challenge: %v", err)
	}
	settingsExec(t, f.db, `update certificates set challenge = 'none' where id = ?`, ss.ID)
	before, _, beforeSecrets := f.rows(t)
	noKey := NewSQLiteCertificateIssueStore(f.db, settingsWriter(f.db), nil)
	if _, err := noKey.RenewSelfSignedCertificateByID(ctx, ss.ID); err == nil || err.Error() != "self-signed renewal requires a master key" {
		t.Fatalf("no master key: %v", err)
	}
	if after, _, afterSecrets := f.rows(t); after != before || afterSecrets != beforeSecrets {
		t.Fatal("guard failures must write nothing")
	}
}

func TestSQLiteCertificateRenewEnqueuesApplyJobForFirstActiveOwner(t *testing.T) {
	f := newIssueFixture(t)
	f.seedOwner(t, issueOwnerD, "owner", 0, "0")    // inactive owner, oldest
	f.seedOwner(t, issueOwnerC, "operator", 1, "0") // not an owner
	f.seedOwner(t, issueOwnerB, "owner", 1, *certKey(t, 400, 0))
	f.seedOwner(t, issueOwnerA, "owner", 1, *certKey(t, 500, 0))
	cert := f.issueSelfSigned(t, "r.test")

	result, err := f.issuer.RenewSelfSignedCertificateByID(context.Background(), cert.ID)
	if err != nil || !result.Applied || !issueUUIDPattern.MatchString(result.JobID) {
		t.Fatalf("apply not queued: %v applied=%v", err, result.Applied)
	}
	var actor, status string
	if err := f.db.QueryRow(`select actor_user_id, status from apply_jobs where id = ?`, result.JobID).Scan(&actor, &status); err != nil {
		t.Fatal(err)
	}
	if actor != issueOwnerB || status != "queued" || f.count(t, `select count(*) from apply_jobs`) != 1 {
		t.Fatalf("job actor=%s status=%s", actor, status)
	}
}

func TestSQLiteCertificateRenewKeepsRenewalWhenEnqueueFails(t *testing.T) {
	f := newIssueFixture(t)
	f.seedOwner(t, issueOwnerA, "owner", 1, "0")
	cert := f.issueSelfSigned(t, "r.test")
	settingsExec(t, f.db, `update installation_settings set default_resolver_pool = null`)

	result, err := f.issuer.RenewSelfSignedCertificateByID(context.Background(), cert.ID)
	if err == nil || err.Error() != "certificate renewed but apply enqueue failed: Configure a default resolver pool before apply" {
		t.Fatalf("error = %v", err)
	}
	// PG parity: the renewal stays committed and is still returned.
	var rowSecret string
	if err := f.db.QueryRow(`select key_secret_id from certificates where id = ?`, cert.ID).Scan(&rowSecret); err != nil {
		t.Fatal(err)
	}
	if result.Applied || result.JobID != "" || result.Certificate.SecretID != rowSecret || rowSecret == cert.SecretID {
		t.Fatal("renewal must persist and be reported even when enqueue fails")
	}
	if f.count(t, `select count(*) from apply_jobs`) != 0 || f.count(t, `select count(*) from config_revisions`) != 0 {
		t.Fatal("failed enqueue must leave no job or revision")
	}
}

func TestSQLiteCertificateRenewReportsOwnerLookupFailure(t *testing.T) {
	f := newIssueFixture(t)
	cert := f.issueSelfSigned(t, "r.test")
	settingsExec(t, f.db, `drop table sessions`)
	settingsExec(t, f.db, `drop table users`)
	result, err := f.issuer.RenewSelfSignedCertificateByID(context.Background(), cert.ID)
	if err == nil || !strings.HasPrefix(err.Error(), "certificate renewed but could not enqueue apply: ") || result.Certificate.ID != cert.ID || result.Applied {
		t.Fatalf("owner lookup failure: %v", err)
	}
}

func TestSQLiteCertificatePersistRenewedIsAtomicAndGuarded(t *testing.T) {
	f := newIssueFixture(t)
	ctx := context.Background()
	cert := f.issueSelfSigned(t, "r.test")
	material, err := acme.CreateInternalCA(0) // any valid material: only the Write matters here
	if err != nil {
		t.Fatal(err)
	}
	_, _, secretsBefore := f.rows(t)

	// The caller's struct says active, but the row moved on: PG reports it and
	// leaves an orphan secret; SQLite rolls the new secret back with the update.
	settingsExec(t, f.db, `update certificates set status = 'failed' where id = ?`, cert.ID)
	if _, err := f.issuer.persistRenewedCertificate(ctx, cert, material); err == nil || err.Error() != "certificate "+cert.ID+" is no longer active" {
		t.Fatalf("error = %v", err)
	}
	if _, _, after := f.rows(t); after != secretsBefore {
		t.Fatal("guard failure must roll the new secret back")
	}

	// A different issuer than the row's is also a miss.
	settingsExec(t, f.db, `update certificates set status = 'active' where id = ?`, cert.ID)
	wrongIssuer := cert
	wrongIssuer.Issuer = "uploaded"
	if _, err := f.issuer.persistRenewedCertificate(ctx, wrongIssuer, material); err == nil || !strings.HasSuffix(err.Error(), "is no longer active") {
		t.Fatalf("issuer guard: %v", err)
	}
}

func TestSQLiteCertificateRenewMarksFailureReasonOnIssueAndSecretFailures(t *testing.T) {
	f := newIssueFixture(t)
	ctx := context.Background()
	cert := f.issueSelfSigned(t, "r.test")
	reason := func() string {
		var value sql.NullString
		if err := f.db.QueryRow(`select failure_reason from certificates where id = ?`, cert.ID).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value.String
	}

	// Leaf issuance fails (CA key cannot be decrypted with another master key).
	otherKey := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("z", 32)))
	wrongKey := NewSQLiteCertificateIssueStore(f.db, settingsWriter(f.db), secrets.NewSQLiteStore(f.db, otherKey))
	_, err := wrongKey.RenewSelfSignedCertificateByID(ctx, cert.ID)
	if err == nil || reason() != err.Error() {
		t.Fatalf("issue failure not recorded: err=%v", err)
	}
	// Secret write fails inside persist.
	settingsExec(t, f.db, `update certificates set failure_reason = null where id = ?`, cert.ID)
	boom := errors.New("injected secret failure")
	failing := NewSQLiteCertificateIssueStore(f.db, settingsWriter(f.db), failCertSecrets{SQLiteStore: f.secrets, err: boom})
	if _, err := failing.RenewSelfSignedCertificateByID(ctx, cert.ID); !errors.Is(err, boom) || reason() != boom.Error() {
		t.Fatalf("secret failure not recorded: err=%v", err)
	}
	stored, err := f.issuer.GetCertificate(ctx, cert.ID)
	if err != nil || stored.SecretID != cert.SecretID || stored.Status != "active" {
		t.Fatal("failed renewal must keep the previous material active")
	}
	// Only active rows are marked.
	settingsExec(t, f.db, `update certificates set failure_reason = null, status = 'failed' where id = ?`, cert.ID)
	if err := f.issuer.markRenewalFailure(ctx, cert.ID, "x"); err != nil || reason() != "" {
		t.Fatalf("inactive row was marked: %v", err)
	}
}

func TestSQLiteCertificateCreateApplyJobStandaloneWrapper(t *testing.T) {
	f := newIssueFixture(t)
	f.seedOwner(t, issueOwnerA, "owner", 1, "0")
	result, err := sqliteCreateApplyJob(context.Background(), settingsWriter(f.db), issueOwnerA)
	if err != nil || result.Job.ID == "" || result.RevisionID == "" || result.Job.Status != "queued" {
		t.Fatalf("wrapper: %v", err)
	}
	if f.count(t, `select count(*) from apply_jobs`) != 1 {
		t.Fatal("expected one job")
	}
	settingsExec(t, f.db, `delete from apply_jobs`)
	settingsExec(t, f.db, `update installation_settings set default_resolver_pool = null`)
	if _, err := sqliteCreateApplyJob(context.Background(), settingsWriter(f.db), issueOwnerA); err == nil || err.Error() != "Configure a default resolver pool before apply" {
		t.Fatalf("no pool: %v", err)
	}
	if f.count(t, `select count(*) from apply_jobs`) != 0 {
		t.Fatal("failed wrapper left a job")
	}
}
