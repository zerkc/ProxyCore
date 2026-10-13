package configuration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/acme"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// Behavior below was observed on postgres:17-alpine against Store.IssueCertificate
// and Store.resolveCloudflareCredentials (see the T56d entry in the task doc).
// The ACME client is always a fake: no network. Tests assert by structure,
// booleans and fingerprints only; no key, token, PEM or ciphertext text is
// ever printed.

const (
	cfToken        = "fake-cf-token-0123456789"
	reasonNone     = "Let's Encrypt requires HTTP-01 or DNS-01"
	reasonNoMaster = "Let's Encrypt issuance requires a master key"
	reasonTimeout  = "Timed out waiting for the HTTP-01 challenge route"
	reasonNoPool   = "Configure a default resolver pool before apply"
	connPurpose    = "cloudflare-api-token"
)

var (
	sharedCAOnce sync.Once
	sharedCA     acme.Material
	sharedCAErr  error
)

// fakeLeaf signs a short lived leaf with one process-wide throwaway CA.
func fakeLeaf(t testing.TB, hostnames []string, validityDays int) acme.Material {
	t.Helper()
	sharedCAOnce.Do(func() { sharedCA, sharedCAErr = acme.CreateInternalCA(0) })
	if sharedCAErr != nil {
		t.Fatal(sharedCAErr)
	}
	material, err := acme.IssueSignedByCA(hostnames, validityDays, sharedCA.CertificatePEM, sharedCA.PrivateKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return material
}

// acmeFake records every ACME call; fn overrides the default success.
type acmeFake struct {
	t     testing.TB
	mu    sync.Mutex
	calls []acme.LetsEncryptOptions
	fn    func(context.Context, acme.LetsEncryptOptions) (acme.Material, error)
}

func (a *acmeFake) issue(ctx context.Context, o acme.LetsEncryptOptions) (acme.Material, error) {
	a.mu.Lock()
	a.calls = append(a.calls, o)
	fn := a.fn
	a.mu.Unlock()
	if fn != nil {
		return fn(ctx, o)
	}
	return fakeLeaf(a.t, o.Hostnames, 90), nil
}

func (a *acmeFake) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

func (a *acmeFake) last(t testing.TB) acme.LetsEncryptOptions {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.calls) == 0 {
		t.Fatal("ACME was not called")
	}
	return a.calls[len(a.calls)-1]
}

type acmeFixture struct {
	*issueFixture
	fake *acmeFake
}

func newACMEFixture(t *testing.T) *acmeFixture {
	t.Helper()
	f := &acmeFixture{issueFixture: newIssueFixture(t), fake: &acmeFake{t: t}}
	f.useFake()
	return f
}

func (f *acmeFixture) useFake() {
	f.issuer.issueACME = f.fake.issue
	f.issuer.jobTimeout = 3 * time.Second
	f.issuer.jobPoll = 5 * time.Millisecond
}

func (f *acmeFixture) issueLE(t *testing.T, challenge, actor string, edit func(*CertificateIssueInput)) (domain.CertificateStatus, error) {
	t.Helper()
	input := CertificateIssueInput{Hostnames: []string{"le.example.test"}, Issuer: "letsencrypt", Challenge: challenge}
	if edit != nil {
		edit(&input)
	}
	return f.issuer.IssueCertificate(context.Background(), input, actor)
}

func (f *acmeFixture) onlyCertificate(t *testing.T) domain.CertificateStatus {
	t.Helper()
	list, err := f.issuer.ListCertificates(context.Background())
	if err != nil || len(list) != 1 {
		t.Fatalf("expected exactly one certificate row: n=%d err=%v", len(list), err)
	}
	return list[0]
}

// assertFailedRow pins the PG failed-row shape: status failed, the reason text,
// and no key, PEM, expiry or renewal date.
func (f *acmeFixture) assertFailedRow(t *testing.T, got domain.CertificateStatus, err error, reason, challenge string) {
	t.Helper()
	if err != nil {
		t.Fatalf("a failed Let's Encrypt issue returns the failed row and a nil error, got %v", err)
	}
	if got.Status != "failed" || got.FailureReason != reason || got.Issuer != "letsencrypt" || got.Challenge != challenge ||
		got.SecretID != "" || got.CertificatePEM != "" || got.ExpiresAt != nil || got.RenewAfter != nil || !issueUUIDPattern.MatchString(got.ID) {
		t.Fatalf("failed row shape differs: status=%q reasonMatches=%v secret=%v pem=%v exp=%v renew=%v", got.Status, got.FailureReason == reason, got.SecretID != "", got.CertificatePEM != "", got.ExpiresAt != nil, got.RenewAfter != nil)
	}
	if !reflect.DeepEqual(f.onlyCertificate(t), got) {
		t.Fatal("returned failed row differs from the stored row")
	}
	if _, keys, _ := f.rows(t); keys != 0 {
		t.Fatal("a failed row must not leave a certificate key secret")
	}
}

func TestSQLiteCertificateIssueLetsEncryptSuccessRowMatchesPG(t *testing.T) {
	f := newACMEFixture(t)
	hostnames := []string{"Le.Example.TEST", "*.wild.test"}
	port := 45
	cert, err := f.issueLE(t, "http-01", "", func(i *CertificateIssueInput) {
		i.Hostnames, i.Email, i.KeyType, i.DirectoryURL, i.PropagationSeconds = hostnames, "ops@example.test", "ecdsa", "https://acme.invalid/dir", &port
	})
	if err != nil {
		t.Fatal(err)
	}
	if cert.Status != "active" || cert.Issuer != "letsencrypt" || cert.Challenge != "http-01" || cert.Environment != "staging" || cert.FailureReason != "" {
		t.Fatalf("unexpected row: status=%s env=%s", cert.Status, cert.Environment)
	}
	if !issueUUIDPattern.MatchString(cert.ID) || !issueUUIDPattern.MatchString(cert.SecretID) || !reflect.DeepEqual(cert.Hostnames, hostnames) {
		t.Fatal("ids must be lowercase uuids and hostnames stored verbatim")
	}
	if cert.ExpiresAt == nil || cert.RenewAfter == nil || !cert.RenewAfter.Equal(cert.ExpiresAt.Add(-30*24*time.Hour)) {
		t.Fatal("renew_after must be expires_at minus 30 days")
	}
	if !within(*cert.ExpiresAt, time.Now().Add(90*24*time.Hour), time.Minute) {
		t.Fatal("expires_at must come from the issued material")
	}
	if !publicKeyMatches(t, f.issueFixture, cert.SecretID, cert.CertificatePEM) {
		t.Fatal("stored key secret does not pair with the stored certificate")
	}
	if !reflect.DeepEqual(f.onlyCertificate(t), cert) {
		t.Fatal("returned value differs from the stored row")
	}
	var purpose, stamp string
	if err := f.db.QueryRow(`select s.purpose, c.created_at from certificates c join secrets s on s.id = c.key_secret_id where c.id = ?`, cert.ID).Scan(&purpose, &stamp); err != nil {
		t.Fatal(err)
	}
	if purpose != keyPurpose || stamp != *certKey(t, 123, 456) {
		t.Fatal("secret purpose or writer clock differs")
	}
	if certs, keys, all := f.rows(t); certs != 1 || keys != 1 || all != 1 {
		t.Fatalf("expected one certificate and one key secret, got %d/%d/%d", certs, keys, all)
	}
	o := f.fake.last(t)
	if !reflect.DeepEqual(o.Hostnames, hostnames) || o.DirectoryURL != "https://acme.invalid/dir" || o.Email != "ops@example.test" ||
		o.Challenge != "http-01" || o.KeyType != "ecdsa" || o.PropagationSeconds != 45 || o.Http01 != acme.Http01Store(acme.GlobalHttp01Store()) || o.Dns01 != nil {
		t.Fatal("ACME options differ from the input")
	}
	if f.count(t, `select count(*) from apply_jobs`) != 0 {
		t.Fatal("http-01 without an actor must not enqueue an apply job")
	}
}

func TestSQLiteCertificateIssueLetsEncryptEnvironmentAndOptionDefaults(t *testing.T) {
	f := newACMEFixture(t)
	cert, err := f.issueLE(t, "http-01", "", func(i *CertificateIssueInput) { i.Environment = "production" })
	if err != nil || cert.Environment != "production" {
		t.Fatalf("explicit environment must be kept: %v", err)
	}
	o := f.fake.last(t)
	if o.PropagationSeconds != 0 || o.KeyType != "" || o.DirectoryURL != "" || o.Email != "" {
		t.Fatal("unset inputs must reach ACME unchanged")
	}
	if _, err := f.issueLE(t, "dns-01", "", func(i *CertificateIssueInput) { i.Cloudflare = &CloudflareInput{APIToken: cfToken} }); err != nil {
		t.Fatal(err)
	}
	if o := f.fake.last(t); o.Challenge != "dns-01" || o.Http01 != nil || o.Dns01 == nil {
		t.Fatal("dns-01 gets the Cloudflare adapter and no http-01 store")
	}
}

func TestSQLiteCertificateIssueLetsEncryptFailuresRecordFailedRowLikePG(t *testing.T) {
	boom := errors.New("acme exploded")
	for _, test := range []struct {
		name, challenge, reason string
		noMaster                bool
		acmeErr                 error
		calls                   int
	}{
		{name: "challenge none", challenge: "none", reason: reasonNone},
		{name: "no master key", challenge: "http-01", reason: reasonNoMaster, noMaster: true},
		{name: "acme error", challenge: "http-01", reason: boom.Error(), acmeErr: boom, calls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newACMEFixture(t)
			if test.noMaster {
				f.issuer = NewSQLiteCertificateIssueStore(f.db, settingsWriter(f.db), nil)
				f.useFake()
			}
			if test.acmeErr != nil {
				f.fake.fn = func(context.Context, acme.LetsEncryptOptions) (acme.Material, error) {
					return acme.Material{}, test.acmeErr
				}
			}
			got, err := f.issueLE(t, test.challenge, "", nil)
			f.assertFailedRow(t, got, err, test.reason, test.challenge)
			if got.Environment != "staging" {
				t.Fatalf("failed row environment = %q", got.Environment)
			}
			if f.fake.count() != test.calls {
				t.Fatalf("ACME calls = %d, want %d", f.fake.count(), test.calls)
			}
		})
	}
}

func TestSQLiteCertificateIssueTypedNilSecretsMeansNoMasterKey(t *testing.T) {
	f := newACMEFixture(t)
	f.issuer = NewSQLiteCertificateIssueStore(f.db, settingsWriter(f.db), (*secrets.SQLiteStore)(nil))
	f.useFake()
	got, err := f.issueLE(t, "http-01", "", nil)
	f.assertFailedRow(t, got, err, reasonNoMaster, "http-01")
}

func TestSQLiteCertificateIssueNonLetsEncryptIssuersReturnTheErrorAndNoRow(t *testing.T) {
	// PG sends every issuer other than self-signed/uploaded through the Let's
	// Encrypt flow but only records a failed row when the issuer is letsencrypt.
	for _, issuer := range []string{"", "bogus"} {
		for challenge, want := range map[string]string{"none": reasonNone, "http-01": "acme exploded"} {
			f := newACMEFixture(t)
			f.fake.fn = func(context.Context, acme.LetsEncryptOptions) (acme.Material, error) {
				return acme.Material{}, errors.New("acme exploded")
			}
			got, err := f.issuer.IssueCertificate(context.Background(), CertificateIssueInput{Hostnames: []string{"a.test"}, Issuer: issuer, Challenge: challenge}, "")
			if err == nil || err.Error() != want || !reflect.DeepEqual(got, domain.CertificateStatus{}) {
				t.Fatalf("issuer %q challenge %q: %v", issuer, challenge, err)
			}
			if certs, _, all := f.rows(t); certs != 0 || all != 0 {
				t.Fatalf("issuer %q wrote state", issuer)
			}
		}
	}
}

func TestSQLiteCertificateIssueLetsEncryptRowFailureIsAtomicAndReturnsInsertError(t *testing.T) {
	// A NUL byte in environment violates a CHECK after the key secret was
	// inserted, and also fails the failed-row insert: PG then returns that
	// insert error (not the original one) and leaves no certificate row.
	f := newACMEFixture(t)
	if _, err := f.issueLE(t, "http-01", "", func(i *CertificateIssueInput) { i.Environment = "a\x00b" }); err == nil {
		t.Fatal("expected a CHECK failure")
	}
	f.assertNoCertificateState(t)
	if _, _, all := f.rows(t); all != 0 {
		t.Fatal("the secret must roll back with the row")
	}

	f.fake.fn = func(context.Context, acme.LetsEncryptOptions) (acme.Material, error) {
		return acme.Material{}, errors.New("acme exploded")
	}
	_, err := f.issueLE(t, "http-01", "", func(i *CertificateIssueInput) { i.Environment = "a\x00b" })
	if err == nil || err.Error() == "acme exploded" {
		t.Fatalf("the failed-row insert error must be returned, got %v", err)
	}
	f.assertNoCertificateState(t)
}

func TestSQLiteCertificateIssueLetsEncryptSecretFailureBecomesFailedRow(t *testing.T) {
	f := newACMEFixture(t)
	injected := errors.New("secret store down")
	f.issuer = NewSQLiteCertificateIssueStore(f.db, settingsWriter(f.db), failPurposeSecrets{SQLiteStore: f.secrets, purpose: keyPurpose, err: injected})
	f.useFake()
	got, err := f.issueLE(t, "http-01", "", nil)
	f.assertFailedRow(t, got, err, injected.Error(), "http-01")
	if f.fake.count() != 1 {
		t.Fatal("ACME ran before the failing persist")
	}
}

// failPurposeSecrets fails PutInWrite for one purpose only.
type failPurposeSecrets struct {
	*secrets.SQLiteStore
	purpose string
	err     error
}

func (s failPurposeSecrets) PutInWrite(v sqlitedb.WriteValue, purpose, plaintext string) (string, error) {
	if purpose == s.purpose {
		return "", s.err
	}
	return s.SQLiteStore.PutInWrite(v, purpose, plaintext)
}

func TestSQLiteCertificateIssueNeverCallsACMEWhileAWriteIsHeld(t *testing.T) {
	f := newACMEFixture(t)
	var probeErr error
	probed := false
	f.fake.fn = func(ctx context.Context, o acme.LetsEncryptOptions) (acme.Material, error) {
		probed = true
		wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		probeErr = f.issuer.writer.Write(wctx, func(v sqlitedb.WriteValue) error {
			_, err := v.Conn().ExecContext(v.Context(), `update installation_settings set updated_at = updated_at`)
			return err
		})
		return fakeLeaf(t, o.Hostnames, 90), nil
	}
	if _, err := f.issueLE(t, "http-01", "", nil); err != nil {
		t.Fatal(err)
	}
	if !probed || probeErr != nil {
		t.Fatalf("the writer must be free (and not nested) during ACME: probed=%v err=%v", probed, probeErr)
	}
}

func TestSQLiteCertificateIssueCanceledContextDuringACME(t *testing.T) {
	f := newACMEFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.fake.fn = func(ctx context.Context, o acme.LetsEncryptOptions) (acme.Material, error) {
		cancel()
		return acme.Material{}, ctx.Err()
	}
	_, err := f.issuer.IssueCertificate(ctx, CertificateIssueInput{Hostnames: []string{"a.test"}, Issuer: "letsencrypt", Challenge: "http-01"}, "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a canceled context must surface (the failed-row insert cannot run): %v", err)
	}
	f.assertNoCertificateState(t)
}

func TestSQLiteCertificateIssueClosedDB(t *testing.T) {
	f := newACMEFixture(t)
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.issueLE(t, "http-01", "", nil); err == nil {
		t.Fatal("closed database must fail")
	}
}

// ---- http-01 apply job route ----

// resolveJobs flips every queued apply job to status through the writer, from
// another goroutine: it can only succeed while no Write is held by the issuer.
func (f *acmeFixture) resolveJobs(t *testing.T, status string, message any) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			time.Sleep(2 * time.Millisecond)
			_ = settingsWriter(f.db).Write(ctx, func(v sqlitedb.WriteValue) error {
				_, err := v.Conn().ExecContext(v.Context(), `update apply_jobs set status=?, error_message=? where status='queued'`, status, message)
				return err
			})
		}
	}()
	return func() { cancel(); wg.Wait() }
}

func TestSQLiteCertificateIssueHTTP01WithActorWaitsForApplyThenIssues(t *testing.T) {
	f := newACMEFixture(t)
	f.seedOwner(t, issueOwnerA, "owner", 1, "0")
	var jobStatus string
	var jobs int
	f.fake.fn = func(_ context.Context, o acme.LetsEncryptOptions) (acme.Material, error) {
		_ = f.db.QueryRow(`select count(*), coalesce(max(status),'') from apply_jobs`).Scan(&jobs, &jobStatus)
		return fakeLeaf(t, o.Hostnames, 90), nil
	}
	stop := f.resolveJobs(t, "applied", nil)
	defer stop()
	cert, err := f.issueLE(t, "http-01", issueOwnerA, nil)
	if err != nil || cert.Status != "active" {
		t.Fatalf("issue failed: %v", err)
	}
	if jobs != 1 || jobStatus != "applied" {
		t.Fatalf("ACME must run after the apply job is applied: jobs=%d status=%s", jobs, jobStatus)
	}
	var actor, target string
	if err := f.db.QueryRow(`select actor_user_id, target from apply_jobs`).Scan(&actor, &target); err != nil || actor != issueOwnerA || target != "combined" {
		t.Fatalf("apply job actor/target differ: %v", err)
	}
}

func TestSQLiteCertificateIssueHTTP01ApplyOutcomesBecomeFailedRows(t *testing.T) {
	for _, test := range []struct {
		name, status string
		message      any
		reason       string
	}{
		{"failed with message", "failed", "nginx failed", "HTTP-01 challenge route apply failed: nginx failed"},
		{"rolled back without message", "rolled-back", nil, "HTTP-01 challenge route apply failed: rolled-back"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newACMEFixture(t)
			f.seedOwner(t, issueOwnerA, "owner", 1, "0")
			stop := f.resolveJobs(t, test.status, test.message)
			defer stop()
			got, err := f.issueLE(t, "http-01", issueOwnerA, nil)
			f.assertFailedRow(t, got, err, test.reason, "http-01")
			if f.fake.count() != 0 {
				t.Fatal("ACME must not run when the challenge route failed to apply")
			}
		})
	}
}

func TestSQLiteCertificateIssueHTTP01TimesOutWhileJobStaysQueued(t *testing.T) {
	f := newACMEFixture(t)
	f.seedOwner(t, issueOwnerA, "owner", 1, "0")
	f.issuer.jobTimeout = 60 * time.Millisecond
	start := time.Now()
	got, err := f.issueLE(t, "http-01", issueOwnerA, nil)
	f.assertFailedRow(t, got, err, reasonTimeout, "http-01")
	if time.Since(start) < 60*time.Millisecond || time.Since(start) > 5*time.Second || f.fake.count() != 0 {
		t.Fatalf("timeout bound violated: %v", time.Since(start))
	}
}

func TestSQLiteCertificateIssueHTTP01EnqueueFailures(t *testing.T) {
	f := newACMEFixture(t)
	f.seedOwner(t, issueOwnerA, "owner", 1, "0")
	settingsExec(t, f.db, `update installation_settings set default_resolver_pool = null`)
	got, err := f.issueLE(t, "http-01", issueOwnerA, nil)
	f.assertFailedRow(t, got, err, reasonNoPool, "http-01")
	if f.count(t, `select count(*) from apply_jobs`) != 0 || f.fake.count() != 0 {
		t.Fatal("no pool: no job and no ACME")
	}

	g := newACMEFixture(t)
	got, err = g.issueLE(t, "http-01", "not-a-uuid", nil)
	if err != nil || got.Status != "failed" || got.FailureReason == "" || g.fake.count() != 0 || g.count(t, `select count(*) from apply_jobs`) != 0 {
		t.Fatalf("a malformed actor id records a failed row without ACME: %v", err)
	}
}

func TestSQLiteCertificateIssueHTTP01CanceledDuringPollReturnsContextError(t *testing.T) {
	f := newACMEFixture(t)
	f.seedOwner(t, issueOwnerA, "owner", 1, "0")
	f.issuer.jobTimeout = time.Minute
	f.issuer.jobPoll = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for f.count(t, `select count(*) from apply_jobs`) == 0 {
			time.Sleep(2 * time.Millisecond)
		}
		cancel()
	}()
	start := time.Now()
	_, err := f.issuer.IssueCertificate(ctx, CertificateIssueInput{Hostnames: []string{"a.test"}, Issuer: "letsencrypt", Challenge: "http-01"}, issueOwnerA)
	if !errors.Is(err, context.Canceled) || time.Since(start) > 10*time.Second {
		t.Fatalf("cancellation must interrupt the poll: %v after %v", err, time.Since(start))
	}
	f.assertNoCertificateState(t)
	if f.fake.count() != 0 {
		t.Fatal("ACME must not run after cancellation")
	}
}

func TestSQLiteCertificateIssueDefaultJobTimeoutAndPoll(t *testing.T) {
	var s SQLiteCertificateIssueStore
	if s.httpChallengeTimeout() != 60*time.Second || s.httpChallengePoll() != 250*time.Millisecond {
		t.Fatal("production defaults must match PG: 60s timeout, 250ms poll")
	}
	s.jobTimeout, s.jobPoll = time.Second, time.Millisecond
	if s.httpChallengeTimeout() != time.Second || s.httpChallengePoll() != time.Millisecond {
		t.Fatal("seams must override the defaults")
	}
}

// ---- Cloudflare credential resolution ----

func (f *acmeFixture) resolve(t *testing.T, in *CloudflareInput) (token, zoneID, zoneName string, err error) {
	t.Helper()
	return f.issuer.resolveCloudflareCredentials(context.Background(), in)
}

func (f *acmeFixture) credCounts(t *testing.T) (conns, secs int) {
	t.Helper()
	return f.count(t, `select count(*) from provider_connections`), f.count(t, `select count(*) from secrets where purpose = ?`, connPurpose)
}

func tokenName(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "cloudflare-" + hex.EncodeToString(sum[:])
}

func TestSQLiteCloudflareCredentialsMatrixMatchesPG(t *testing.T) {
	f := newACMEFixture(t)
	// No connection and no token.
	for _, in := range []*CloudflareInput{nil, {}, {ZoneID: "z"}} {
		if _, _, _, err := f.resolve(t, in); err == nil || err.Error() != "Cloudflare credentials not configured for this DNS zone" {
			t.Fatalf("no credentials: %v", err)
		}
	}
	// New token: trimmed zone id, trimmed lowercase zone name, one secret and one connection.
	tok, zid, zn, err := f.resolve(t, &CloudflareInput{APIToken: cfToken, ZoneID: "  zid ", ZoneName: " ExAmple.TEST "})
	if err != nil || tok != cfToken || zid != "zid" || zn != "example.test" {
		t.Fatalf("new token: err=%v zid=%q zn=%q tokenOK=%v", err, zid, zn, tok == cfToken)
	}
	if conns, secs := f.credCounts(t); conns != 1 || secs != 1 {
		t.Fatalf("expected one connection and one secret, got %d/%d", conns, secs)
	}
	var id, name, scope string
	var enabled int
	var secretID, created string
	if err := f.db.QueryRow(`select id, name, scope, enabled, secret_id, created_at from provider_connections where provider = 'cloudflare'`).Scan(&id, &name, &scope, &enabled, &secretID, &created); err != nil {
		t.Fatal(err)
	}
	if !issueUUIDPattern.MatchString(id) || name != tokenName(cfToken) || scope != "dns-01" || enabled != 1 || created != *certKey(t, 123, 456) {
		t.Fatal("connection row differs from PG's insert")
	}
	if plain, err := f.secrets.Get(context.Background(), secretID); err != nil || plain != cfToken {
		t.Fatalf("connection secret does not hold the token: %v", err)
	}
	// Same token again: reuse, and the zone fields echo only the new input.
	tok, zid, zn, err = f.resolve(t, &CloudflareInput{APIToken: cfToken})
	if err != nil || tok != cfToken || zid != "" || zn != "" {
		t.Fatalf("existing token: %v", err)
	}
	if conns, secs := f.credCounts(t); conns != 1 || secs != 1 {
		t.Fatal("an existing connection must not be duplicated")
	}
	// A disabled connection is ignored by the token lookup.
	settingsExec(t, f.db, `update provider_connections set enabled = 0`)
	if _, _, _, err := f.resolve(t, &CloudflareInput{APIToken: cfToken}); err != nil {
		t.Fatal(err)
	}
	if conns, secs := f.credCounts(t); conns != 2 || secs != 2 {
		t.Fatalf("a disabled connection must not be reused, got %d/%d", conns, secs)
	}
	// A different scope with the same name is not a dns-01 match either.
	settingsExec(t, f.db, `update provider_connections set scope = 'other' where enabled = 1`)
	if _, _, _, err := f.resolve(t, &CloudflareInput{APIToken: cfToken}); err != nil {
		t.Fatal(err)
	}
	if conns, _ := f.credCounts(t); conns != 3 {
		t.Fatal("scope must be part of the token lookup")
	}
}

func TestSQLiteCloudflareCredentialsWithoutTokenUseNewestEnabledConnection(t *testing.T) {
	f := newACMEFixture(t)
	ctx := context.Background()
	oldSecret, _ := f.secrets.Put(ctx, connPurpose, "old-token")
	newSecret, _ := f.secrets.Put(ctx, connPurpose, "new-token")
	offSecret, _ := f.secrets.Put(ctx, connPurpose, "off-token")
	add := func(name, scope, secretID string, enabled int, created int64) {
		settingsExec(t, f.db, `insert into provider_connections (id,provider,name,secret_id,scope,enabled,created_at,updated_at)
			values (?,'cloudflare',?,?,?,?,?,?)`, "dddddddd-0000-4000-8000-0000000000"+fmt.Sprintf("%02d", created), name, secretID, scope, enabled, *certKey(t, created, 0), *certKey(t, created, 0))
	}
	add("a", "dns-01", oldSecret, 1, 10)
	add("n-new", "zone-x", newSecret, 1, 20)
	add("n-off", "dns-01", offSecret, 0, 30)
	// Newest enabled wins; non dns-01 scope maps name to zoneID and scope to zoneName.
	tok, zid, zn, err := f.resolve(t, nil)
	if err != nil || tok != "new-token" || zid != "n-new" || zn != "zone-x" {
		t.Fatalf("newest enabled: err=%v zid=%q zn=%q", err, zid, zn)
	}
	// An empty input behaves like nil: no zone echo.
	if tok, zid, zn, err = f.resolve(t, &CloudflareInput{ZoneID: "ignored", ZoneName: "ignored"}); err != nil || tok != "new-token" || zid != "n-new" || zn != "zone-x" {
		t.Fatalf("zone input must be ignored without a token: %v", err)
	}
	// dns-01 scope returns no zone fallbacks.
	settingsExec(t, f.db, `update provider_connections set scope = 'dns-01' where name = 'n-new'`)
	if tok, zid, zn, err = f.resolve(t, nil); err != nil || tok != "new-token" || zid != "" || zn != "" {
		t.Fatalf("dns-01 scope: err=%v zid=%q zn=%q", err, zid, zn)
	}
	if conns, _ := f.credCounts(t); conns != 3 {
		t.Fatal("reading credentials must not create connections")
	}
	// Other providers are ignored.
	settingsExec(t, f.db, `update provider_connections set provider = 'other'`)
	if _, _, _, err = f.resolve(t, nil); err == nil || err.Error() != "Cloudflare credentials not configured for this DNS zone" {
		t.Fatalf("provider filter: %v", err)
	}
}

func TestSQLiteCloudflareCredentialsSecretProblems(t *testing.T) {
	f := newACMEFixture(t)
	ctx := context.Background()
	// A connection whose secret row is gone (only possible with foreign keys
	// off) reads as "" and is reported as unavailable.
	conn, err := f.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `pragma foreign_keys = off`); err != nil {
		t.Fatal(err)
	}
	_, err = conn.ExecContext(ctx, `insert into provider_connections (id,provider,name,secret_id,scope,enabled,created_at,updated_at)
		values ('dddddddd-0000-4000-8000-0000000000e1','cloudflare','n','eeeeeeee-0000-4000-8000-0000000000e1','dns-01',1,'0','0')`)
	_, _ = conn.ExecContext(ctx, `pragma foreign_keys = on`)
	conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := f.resolve(t, nil); err == nil || err.Error() != "Cloudflare credential secret is unavailable" {
		t.Fatalf("missing secret: %v", err)
	}
	emptySecret, err := f.secrets.Put(ctx, connPurpose, "")
	if err != nil {
		t.Fatal(err)
	}
	settingsExec(t, f.db, `update provider_connections set secret_id = ?`, emptySecret)
	settingsExec(t, f.db, `update secrets set ciphertext = 'not-encrypted' where id = ?`, emptySecret)
	if _, _, _, err := f.resolve(t, nil); err == nil || err.Error() != "Unsupported encrypted secret format" {
		t.Fatalf("corrupt ciphertext: %v", err)
	}
	g := newACMEFixture(t)
	g.issuer = NewSQLiteCertificateIssueStore(g.db, settingsWriter(g.db), nil)
	if _, _, _, err := g.resolve(t, &CloudflareInput{APIToken: cfToken}); err == nil || err.Error() != "Cloudflare DNS-01 requires a master key" {
		t.Fatalf("no master key: %v", err)
	}
	if conns, secs := g.credCounts(t); conns != 0 || secs != 0 {
		t.Fatal("no master key must write nothing")
	}
}

func TestSQLiteCloudflareCredentialsAreAtomic(t *testing.T) {
	// Secret insert fails: nothing is written.
	f := newACMEFixture(t)
	f.issuer = NewSQLiteCertificateIssueStore(f.db, settingsWriter(f.db), failPurposeSecrets{SQLiteStore: f.secrets, purpose: connPurpose, err: errors.New("secret store down")})
	if _, _, _, err := f.resolve(t, &CloudflareInput{APIToken: cfToken}); err == nil || err.Error() != "secret store down" {
		t.Fatalf("secret failure: %v", err)
	}
	if conns, secs := f.credCounts(t); conns != 0 || secs != 0 {
		t.Fatal("failed credential persist left state")
	}
	// Connection insert fails after the secret was inserted: PG leaves an orphan
	// secret; SQLite rolls both back.
	g := newACMEFixture(t)
	settingsExec(t, g.db, `create trigger deny_connection before insert on provider_connections begin select raise(abort, 'connection denied'); end`)
	if _, _, _, err := g.resolve(t, &CloudflareInput{APIToken: cfToken}); err == nil || !strings.Contains(err.Error(), "connection denied") {
		t.Fatalf("connection failure: %v", err)
	}
	if conns, secs := g.credCounts(t); conns != 0 || secs != 0 {
		t.Fatalf("orphan secret after a failed connection insert: %d/%d", conns, secs)
	}
}

func TestSQLiteCloudflareConcurrentResolvesConvergeOnOneConnection(t *testing.T) {
	f := newACMEFixture(t)
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _, err := f.resolve(t, &CloudflareInput{APIToken: cfToken})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if conns, secs := f.credCounts(t); conns != 1 || secs != 1 {
		t.Fatalf("concurrent resolves must converge: %d connections, %d secrets (PG would duplicate)", conns, secs)
	}
}

func TestSQLiteCertificateIssueDNS01CredentialFailuresBecomeFailedRows(t *testing.T) {
	f := newACMEFixture(t)
	got, err := f.issueLE(t, "dns-01", "", nil)
	f.assertFailedRow(t, got, err, "Cloudflare credentials not configured for this DNS zone", "dns-01")
	if f.fake.count() != 0 {
		t.Fatal("ACME must not run without credentials")
	}
}

func TestSQLiteCertificateIssueDNS01KeepsConnectionWhenACMEFails(t *testing.T) {
	f := newACMEFixture(t)
	f.fake.fn = func(context.Context, acme.LetsEncryptOptions) (acme.Material, error) {
		return acme.Material{}, errors.New("acme exploded")
	}
	got, err := f.issueLE(t, "dns-01", "", func(i *CertificateIssueInput) {
		i.Environment, i.Cloudflare = "production", &CloudflareInput{APIToken: cfToken}
	})
	f.assertFailedRow(t, got, err, "acme exploded", "dns-01")
	if got.Environment != "production" {
		t.Fatal("failed row keeps the requested environment")
	}
	if conns, secs := f.credCounts(t); conns != 1 || secs != 1 {
		t.Fatal("PG keeps the connection created before the ACME failure")
	}
}

// SQLite-only: PG leaves created_at ties unspecified; SQLite breaks them by
// insertion order so the newest connection is deterministic.
func TestSQLiteCloudflareCredentialsCreatedAtTiesPreferTheLastInserted(t *testing.T) {
	f := newACMEFixture(t)
	ctx := context.Background()
	for i, token := range []string{"tie-first", "tie-second", "tie-third"} {
		secretID, err := f.secrets.Put(ctx, connPurpose, token)
		if err != nil {
			t.Fatal(err)
		}
		settingsExec(t, f.db, `insert into provider_connections (id,provider,name,secret_id,scope,enabled,created_at,updated_at)
			values (?,'cloudflare',?,?,'dns-01',1,'0','0')`, fmt.Sprintf("dddddddd-0000-4000-8000-0000000000f%d", i), "n", secretID)
	}
	if tok, _, _, err := f.resolve(t, nil); err != nil || tok != "tie-third" {
		t.Fatalf("the last inserted connection must win a created_at tie: %v", err)
	}
	if tok, _, _, err := f.resolve(t, &CloudflareInput{APIToken: "tie-first"}); err != nil || tok != "tie-first" {
		t.Fatalf("token lookup: %v", err)
	}
}

// Deterministic version of the race: another caller creates the connection
// between this caller's lookup miss and its Write. The Write must notice and
// reuse it instead of adding a second secret and connection.
func TestSQLiteCloudflareCredentialsWriteRechecksAfterALookupMiss(t *testing.T) {
	f := newACMEFixture(t)
	other := f.issuer
	f.issuer.afterCredentialMiss = func() {
		f.issuer.afterCredentialMiss = nil
		if _, _, _, err := other.resolveCloudflareCredentials(context.Background(), &CloudflareInput{APIToken: cfToken}); err != nil {
			t.Error(err)
		}
	}
	if tok, _, _, err := f.resolve(t, &CloudflareInput{APIToken: cfToken}); err != nil || tok != cfToken {
		t.Fatal(err)
	}
	if conns, secs := f.credCounts(t); conns != 1 || secs != 1 {
		t.Fatalf("a concurrent creation must be reused: %d connections, %d secrets", conns, secs)
	}
}
