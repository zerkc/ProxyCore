package configuration

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/acme"
	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// Tests assert by structure, fingerprints and booleans only: key material, PEM
// bodies and ciphertext are never put into failure messages.

type caFixture struct {
	db      *sql.DB
	secrets *secrets.SQLiteStore
	store   *SQLiteInternalCAStore
}

func newCAFixture(t *testing.T) *caFixture {
	t.Helper()
	db, _, _ := sqliteSettingsFixture(t)
	sec := secrets.NewSQLiteStore(db, recordMasterKey)
	return &caFixture{db: db, secrets: sec, store: NewSQLiteInternalCAStore(db, settingsWriter(db), sec)}
}

func (f *caFixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *caFixture) assertEmpty(t *testing.T) {
	t.Helper()
	if rows, secs := f.count(t, `select count(*) from internal_ca`), f.count(t, `select count(*) from secrets`); rows != 0 || secs != 0 {
		t.Fatalf("expected no CA state, got internal_ca=%d secrets=%d", rows, secs)
	}
}

func parseCertPEM(t *testing.T, value string) *x509.Certificate {
	t.Helper()
	block, rest := pem.Decode([]byte(value))
	if block == nil || block.Type != "CERTIFICATE" || len(rest) != 0 {
		t.Fatal("not exactly one CERTIFICATE PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func within(got, want time.Time, slack time.Duration) bool {
	d := got.Sub(want)
	return d < slack && d > -slack
}

func TestSQLiteInternalCAEnsureCreatesOnceAndIsIdempotent(t *testing.T) {
	f := newCAFixture(t)
	ctx := context.Background()
	first, err := f.store.EnsureInternalCACertificatePEM(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.store.EnsureInternalCACertificatePEM(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("repeated ensure returned a different CA")
	}
	if rows, secs := f.count(t, `select count(*) from internal_ca`), f.count(t, `select count(*) from secrets where purpose = 'internal-ca-private-key'`); rows != 1 || secs != 1 || f.count(t, `select count(*) from secrets`) != 1 {
		t.Fatalf("internal_ca=%d ca secrets=%d", rows, secs)
	}

	ca := parseCertPEM(t, first)
	now := time.Now()
	wantKU := x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature
	if !ca.IsCA || !ca.MaxPathLenZero || ca.KeyUsage != wantKU || ca.Subject.CommonName != "ProxyCore Internal CA" ||
		len(ca.Subject.Organization) != 1 || ca.Subject.Organization[0] != "ProxyCore" ||
		ca.SignatureAlgorithm != x509.SHA256WithRSA || ca.CheckSignatureFrom(ca) != nil {
		t.Fatal("CA certificate structure differs from the PG CA")
	}
	if !within(ca.NotAfter, now.Add(3650*24*time.Hour), time.Minute) || !within(ca.NotBefore, now.Add(-time.Hour), time.Minute) {
		t.Fatal("CA validity window is not 10 years starting one hour ago")
	}

	var secretID, createdAt, updatedAt, certColumn string
	if err := f.db.QueryRow(`select key_secret_id, created_at, updated_at from internal_ca where id = 'default'`).
		Scan(&secretID, &createdAt, &updatedAt); err != nil {
		t.Fatal(err)
	}
	stamp := *certKey(t, 123, 456)
	if createdAt != stamp || updatedAt != stamp || secretID != strings.ToLower(secretID) {
		t.Fatal("internal_ca row does not carry the writer's tagged clock and a lowercase id")
	}
	if err := f.db.QueryRow(`select certificate_pem from internal_ca`).Scan(&certColumn); err != nil || certColumn != first {
		t.Fatal("stored certificate differs from the returned one")
	}

	keyPEM, err := f.secrets.Get(ctx, secretID)
	if err != nil {
		t.Fatal(err)
	}
	key, err := acme.ParseRSAPrivateKeyPEM(keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	if pub, ok := ca.PublicKey.(*rsa.PublicKey); !ok || !pub.Equal(&key.PublicKey) {
		t.Fatal("stored key does not match the CA certificate")
	}
	var ciphertext string
	if err := f.db.QueryRow(`select ciphertext from secrets where id = ?`, secretID).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if ciphertext == keyPEM || strings.Contains(ciphertext, "PRIVATE KEY") || strings.Contains(ciphertext, "BEGIN") {
		t.Fatal("CA key is stored unencrypted")
	}
}

func TestSQLiteInternalCAConcurrentFirstEnsureConverges(t *testing.T) {
	f := newCAFixture(t)
	const callers = 8
	results := make([]string, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = f.store.EnsureInternalCACertificatePEM(context.Background())
		}()
	}
	wg.Wait()
	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if results[i] != results[0] {
			t.Fatalf("caller %d returned a different CA", i)
		}
	}
	var stored string
	if err := f.db.QueryRow(`select certificate_pem from internal_ca`).Scan(&stored); err != nil || stored != results[0] {
		t.Fatal("returned CA is not the stored winner")
	}
	if rows, secs := f.count(t, `select count(*) from internal_ca`), f.count(t, `select count(*) from secrets`); rows != 1 || secs != 1 {
		t.Fatalf("internal_ca=%d secrets=%d, want 1 and 1 (no orphan key secrets)", rows, secs)
	}
}

func TestSQLiteInternalCAIssueLeaf(t *testing.T) {
	f := newCAFixture(t)
	ctx := context.Background()
	hosts := []string{"a.example.test", "192.0.2.5", "*.b.example.test"}
	material, err := f.store.issueInternalLeaf(ctx, hosts, 365)
	if err != nil {
		t.Fatal(err)
	}
	caPEM, err := f.store.EnsureInternalCACertificatePEM(ctx)
	if err != nil {
		t.Fatal(err)
	}
	leaf, ca := parseCertPEM(t, material.CertificatePEM), parseCertPEM(t, caPEM)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	for _, name := range []string{"a.example.test", "192.0.2.5", "x.b.example.test"} {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: name, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
			t.Fatalf("leaf does not verify for %s: %v", name, err)
		}
	}
	if len(leaf.DNSNames) != 2 || len(leaf.IPAddresses) != 1 || leaf.IPAddresses[0].String() != "192.0.2.5" || leaf.Subject.CommonName != "a.example.test" {
		t.Fatal("leaf subject/SAN differ from the requested hostnames")
	}
	if leaf.IsCA || leaf.KeyUsage != x509.KeyUsageDigitalSignature|x509.KeyUsageKeyEncipherment ||
		len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth || leaf.SignatureAlgorithm != x509.SHA256WithRSA {
		t.Fatal("leaf key usages or CA flag are wrong")
	}
	now := time.Now()
	if !within(leaf.NotAfter, now.Add(365*24*time.Hour), time.Minute) || !within(leaf.NotBefore, now.Add(-5*time.Minute), time.Minute) ||
		!within(material.ExpiresAt, leaf.NotAfter, 2*time.Second) {
		t.Fatal("leaf validity is not 365 days from now")
	}
	key, err := acme.ParseRSAPrivateKeyPEM(material.PrivateKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	if pub, ok := leaf.PublicKey.(*rsa.PublicKey); !ok || !pub.Equal(&key.PublicKey) {
		t.Fatal("leaf key does not match the leaf certificate")
	}
	if secs := f.count(t, `select count(*) from secrets`); secs != 1 {
		t.Fatalf("issuing a leaf must not store anything: secrets=%d", secs)
	}
	if _, err := f.store.issueInternalLeaf(ctx, nil, 365); err == nil || err.Error() != "At least one certificate hostname is required" {
		t.Fatalf("empty hostnames err=%v", err)
	}
}

func TestSQLiteInternalCARequiresMasterKey(t *testing.T) {
	f := newCAFixture(t)
	var typedNil *secrets.SQLiteStore
	for name, store := range map[string]*SQLiteInternalCAStore{
		"nil":       NewSQLiteInternalCAStore(f.db, settingsWriter(f.db), nil),
		"typed nil": NewSQLiteInternalCAStore(f.db, settingsWriter(f.db), typedNil),
	} {
		if _, err := store.EnsureInternalCACertificatePEM(context.Background()); err == nil || err.Error() != "internal CA requires a master key" {
			t.Fatalf("%s ensure err=%v", name, err)
		}
		if _, err := store.issueInternalLeaf(context.Background(), []string{"a.example.test"}, 365); err == nil || err.Error() != "internal CA requires a master key" {
			t.Fatalf("%s leaf err=%v", name, err)
		}
	}
	f.assertEmpty(t)
}

// faultySecrets stores through the real secret store, then fails or returns a
// malformed id, so the surrounding Write must roll the stored secret back.
type faultySecrets struct {
	*secrets.SQLiteStore
	failAfterPut error
	badID        bool
}

func (s faultySecrets) PutInWrite(v sqlitedb.WriteValue, purpose, plaintext string) (string, error) {
	id, err := s.SQLiteStore.PutInWrite(v, purpose, plaintext)
	if err != nil {
		return "", err
	}
	if s.failAfterPut != nil {
		return "", s.failAfterPut
	}
	if s.badID {
		return strings.ToUpper(id), nil // violates the lowercase-uuid CHECK
	}
	return id, nil
}

func TestSQLiteInternalCARollsBackSecretOnFailure(t *testing.T) {
	boom := errors.New("injected")
	for name, secret := range map[string]faultySecrets{
		"secret writer fails after insert": {failAfterPut: boom},
		"internal_ca CHECK violation":      {badID: true},
	} {
		f := newCAFixture(t)
		secret.SQLiteStore = f.secrets
		store := NewSQLiteInternalCAStore(f.db, settingsWriter(f.db), secret)
		_, err := store.EnsureInternalCACertificatePEM(context.Background())
		if err == nil || (secret.failAfterPut != nil && !errors.Is(err, boom)) || (secret.badID && !strings.Contains(err.Error(), "persist internal CA: ")) {
			t.Fatalf("%s: err=%v", name, err)
		}
		f.assertEmpty(t)
		// The store recovers: a healthy ensure afterwards succeeds.
		if _, err := f.store.EnsureInternalCACertificatePEM(context.Background()); err != nil {
			t.Fatalf("%s: recovery: %v", name, err)
		}
	}
}

func TestSQLiteInternalCARejectsBrokenEstablishedMaterial(t *testing.T) {
	ctx := context.Background()
	other, err := acme.CreateInternalCA(0)
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := acme.CreateInternalCA(0)
	if err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct{ cert, plain, want string }{
		"blank certificate": {cert: " \n", plain: other.PrivateKeyPEM, want: "established internal CA material is incomplete"},
		"blank key secret":  {cert: other.CertificatePEM, plain: " ", want: "internal CA private key secret is missing"},
		"key mismatch":      {cert: other.CertificatePEM, plain: stranger.PrivateKeyPEM, want: "validate established internal CA: internal CA certificate and private key do not match"},
	} {
		f := newCAFixture(t)
		id, err := f.secrets.Put(ctx, "internal-ca-private-key", test.plain)
		if err != nil {
			t.Fatal(err)
		}
		settingsExec(t, f.db, `insert into internal_ca (id,certificate_pem,key_secret_id,created_at,updated_at) values ('default',?,?,'0','0')`, test.cert, id)
		if _, err := f.store.EnsureInternalCACertificatePEM(ctx); err == nil || err.Error() != test.want {
			t.Fatalf("%s: err=%v", name, err)
		}
		if f.count(t, `select count(*) from internal_ca`) != 1 || f.count(t, `select count(*) from secrets`) != 1 {
			t.Fatalf("%s: broken material must not be replaced", name)
		}
	}
}

func TestSQLiteInternalCAEnsureReturnsExistingCA(t *testing.T) {
	f := newCAFixture(t)
	material, err := acme.CreateInternalCA(0)
	if err != nil {
		t.Fatal(err)
	}
	id, err := f.secrets.Put(context.Background(), "internal-ca-private-key", material.PrivateKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	settingsExec(t, f.db, `insert into internal_ca (id,certificate_pem,key_secret_id,created_at,updated_at) values ('default',?,?,'0','0')`, material.CertificatePEM, id)
	got, err := f.store.EnsureInternalCACertificatePEM(context.Background())
	if err != nil || got != material.CertificatePEM {
		t.Fatalf("existing CA not returned (err=%v)", err)
	}
	if f.count(t, `select count(*) from secrets`) != 1 {
		t.Fatal("ensure created a second secret next to an existing CA")
	}
}

func TestSQLiteInternalCACanceledContextAndClosedDB(t *testing.T) {
	f := newCAFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.store.EnsureInternalCACertificatePEM(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ctx err=%v", err)
	}
	if _, err := f.store.issueInternalLeaf(ctx, []string{"a.example.test"}, 365); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ctx leaf err=%v", err)
	}
	f.assertEmpty(t)
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.EnsureInternalCACertificatePEM(context.Background()); err == nil {
		t.Fatal("closed db must fail")
	}
}

// The key_secret_id CHECK makes a blank id unreachable through normal writes,
// so the row is forced in with checks and foreign keys off on one connection.
func TestSQLiteInternalCARejectsBlankKeySecretID(t *testing.T) {
	ctx := context.Background()
	for name, blank := range map[string]string{"empty": "", "whitespace": " \t\n"} {
		t.Run(name, func(t *testing.T) {
			f := newCAFixture(t)
			other, err := acme.CreateInternalCA(0)
			if err != nil {
				t.Fatal(err)
			}
			conn, err := f.db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, pragma := range []string{"pragma ignore_check_constraints = on", "pragma foreign_keys = off"} {
				if _, err := conn.ExecContext(ctx, pragma); err != nil {
					t.Fatal(err)
				}
			}
			_, insertErr := conn.ExecContext(ctx, `insert into internal_ca (id,certificate_pem,key_secret_id,created_at,updated_at) values ('default',?,?,'0','0')`, other.CertificatePEM, blank)
			_, _ = conn.ExecContext(ctx, "pragma ignore_check_constraints = off")
			_, _ = conn.ExecContext(ctx, "pragma foreign_keys = on")
			conn.Close()
			if insertErr != nil {
				t.Fatal(insertErr)
			}
			if _, err := f.store.EnsureInternalCACertificatePEM(ctx); err == nil || err.Error() != "established internal CA material is incomplete" {
				t.Fatalf("blank key secret id: %v", err)
			}
			if f.count(t, `select count(*) from internal_ca`) != 1 || f.count(t, `select count(*) from secrets`) != 0 {
				t.Fatal("a broken CA row must not be replaced and must not create secrets")
			}
		})
	}
}
