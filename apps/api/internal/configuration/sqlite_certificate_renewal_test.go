package configuration

import (
	"bytes"
	"context"
	"errors"
	"log"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/acme"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// Behavior below was observed on postgres:17-alpine against
// Store.RenewDueCertificates and Store.RenewLetsEncryptCertificate with a faked
// issueLetsEncryptFn. Assertions are structural: no key, PEM or token text.

const (
	renewStaging    = "https://acme-staging.example.test/dir"
	renewProduction = "https://acme-prod.example.test/dir"
	renewEmail      = "ops@example.test"
	renewOldPEM     = "old-pem-placeholder"
	renewReasonDNS  = "Cloudflare credentials not configured for this DNS zone"
)

var (
	renewNow = time.Unix(1_700_000_000, 0).UTC()
	renewDue = int64(1_699_000_000) // before renewNow
	renewFut = int64(1_800_000_000) // after renewNow
)

type renewRow struct {
	status, keySecret, pem, failure, updated string
	renewAfter, expires                      *string
}

func (f *acmeFixture) seedActive(t *testing.T, id, host, issuer, challenge, env string, renew int64, failure any) (oldSecret string) {
	t.Helper()
	old, err := f.secrets.Put(context.Background(), keyPurpose, "old-key-placeholder")
	if err != nil {
		t.Fatal(err)
	}
	settingsExec(t, f.db, `insert into certificates (id,hostnames,issuer,challenge,environment,status,expires_at,renew_after,
		key_secret_id,certificate_pem,failure_reason,created_at,updated_at) values (?,?,?,?,?,'active',?,?,?,?,?,?,?)`,
		id, `["`+host+`"]`, issuer, challenge, env, *certKey(t, renew+86400, 0), *certKey(t, renew, 0), old, renewOldPEM, failure,
		*certKey(t, 1, 0), *certKey(t, 1, 0))
	return old
}

func (f *acmeFixture) row(t *testing.T, id string) renewRow {
	t.Helper()
	var r renewRow
	var failure, secret *string
	if err := f.db.QueryRow(`select status, key_secret_id, certificate_pem, failure_reason, updated_at, renew_after, expires_at from certificates where id = ?`, id).
		Scan(&r.status, &secret, &r.pem, &failure, &r.updated, &r.renewAfter, &r.expires); err != nil {
		t.Fatal(err)
	}
	if secret != nil {
		r.keySecret = *secret
	}
	if failure != nil {
		r.failure = *failure
	}
	return r
}

func (f *acmeFixture) seedPlatform(t *testing.T) {
	t.Helper()
	f.seedOwner(t, issueOwnerA, "owner", 1, "0")
}

func (f *acmeFixture) due(t *testing.T, id string) domain.CertificateStatus {
	t.Helper()
	cert, err := f.issuer.GetCertificate(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func renewOptions() RenewalOptions {
	return RenewalOptions{StagingDirectoryURL: renewStaging, ProductionDirectoryURL: renewProduction, Email: renewEmail}
}

func writableIdentity(role *domain.TopologyRole) RenewalIdentityProvider {
	return func(context.Context) (identity.Identity, bool, error) {
		return identity.Identity{Role: *role, LeadershipGeneration: 1, LatestKnownGeneration: 1}, true, nil
	}
}

// trackingLease records how many work items ran and whether the ACME fake was
// invoked while the lease was held.
type trackingLease struct {
	mu     sync.Mutex
	held   bool
	calls  int
	before func(context.Context) error
}

func (l *trackingLease) WithAutomaticRenewalLease(ctx context.Context, work func(context.Context) error) error {
	l.mu.Lock()
	l.calls++
	before := l.before
	l.mu.Unlock()
	if before != nil {
		if err := before(ctx); err != nil {
			return err
		}
	}
	l.mu.Lock()
	l.held = true
	l.mu.Unlock()
	defer func() { l.mu.Lock(); l.held = false; l.mu.Unlock() }()
	return work(ctx)
}

func (l *trackingLease) isHeld() bool { l.mu.Lock(); defer l.mu.Unlock(); return l.held }

// ---- RenewLetsEncryptCertificate ----

func TestSQLiteRenewLetsEncryptSuccessUpdatesRowLikePG(t *testing.T) {
	f := newACMEFixture(t)
	oldSecret := f.seedActive(t, certIDA, "ok.example.test", "letsencrypt", "http-01", "production", renewDue, "stale failure")
	cert := f.due(t, certIDA)

	got, err := f.issuer.RenewLetsEncryptCertificate(context.Background(), cert, renewOptions())
	if err != nil {
		t.Fatal(err)
	}
	o := f.fake.last(t)
	if o.DirectoryURL != renewProduction || o.KeyType != "rsa" || o.Email != renewEmail || o.Challenge != "http-01" ||
		!reflect.DeepEqual(o.Hostnames, []string{"ok.example.test"}) || o.Http01 != acme.Http01Store(acme.GlobalHttp01Store()) || o.Dns01 != nil || o.PropagationSeconds != 0 {
		t.Fatal("ACME options differ from PG's renewal options")
	}
	if got.Applied || got.JobID != "" {
		t.Fatal("no owner: renewal must not report an apply")
	}
	row := f.row(t, certIDA)
	if row.status != "active" || row.failure != "" || row.pem == renewOldPEM || row.keySecret == oldSecret || row.updated != *certKey(t, 123, 456) {
		t.Fatalf("row not updated: status=%s failureCleared=%v pemChanged=%v keyChanged=%v", row.status, row.failure == "", row.pem != renewOldPEM, row.keySecret != oldSecret)
	}
	if got.Certificate.SecretID != row.keySecret || got.Certificate.RenewAfter == nil || got.Certificate.ExpiresAt == nil ||
		!got.Certificate.RenewAfter.Equal(got.Certificate.ExpiresAt.Add(-30*24*time.Hour)) || !got.Certificate.RenewAfter.After(renewNow) {
		t.Fatal("returned certificate differs from the stored row or renew_after is wrong")
	}
	if !publicKeyMatches(t, f.issueFixture, row.keySecret, row.pem) {
		t.Fatal("new key secret does not pair with the new certificate")
	}
	if plain, err := f.secrets.Get(context.Background(), oldSecret); err != nil || plain == "" {
		t.Fatal("PG leaves the previous key secret in place; so must SQLite")
	}
	if _, keys, _ := f.rows(t); keys != 2 {
		t.Fatalf("expected the old and the new key secret, got %d", keys)
	}
}

func TestSQLiteRenewLetsEncryptDirectorySelectionByEnvironment(t *testing.T) {
	for env, want := range map[string]string{"production": renewProduction, " Production ": renewProduction, "staging": renewStaging, "": renewStaging, "custom": renewStaging, "local": renewStaging} {
		f := newACMEFixture(t)
		f.seedActive(t, certIDA, "a.example.test", "letsencrypt", "http-01", env, renewDue, nil)
		if _, err := f.issuer.RenewLetsEncryptCertificate(context.Background(), f.due(t, certIDA), renewOptions()); err != nil {
			t.Fatal(err)
		}
		if got := f.fake.last(t).DirectoryURL; got != want {
			t.Errorf("environment %q: wrong directory URL selected", env)
		}
	}
}

func TestSQLiteRenewLetsEncryptApplyEnqueueIsBestEffort(t *testing.T) {
	f := newACMEFixture(t)
	f.seedPlatform(t)
	f.seedActive(t, certIDA, "a.example.test", "letsencrypt", "http-01", "staging", renewDue, nil)
	got, err := f.issuer.RenewLetsEncryptCertificate(context.Background(), f.due(t, certIDA), renewOptions())
	if err != nil || !got.Applied || !issueUUIDPattern.MatchString(got.JobID) {
		t.Fatalf("owner present: applied=%v err=%v", got.Applied, err)
	}
	var actor string
	if err := f.db.QueryRow(`select actor_user_id from apply_jobs where id = ?`, got.JobID).Scan(&actor); err != nil || actor != issueOwnerA {
		t.Fatalf("apply job must be queued for the first owner: %v", err)
	}

	// Without a default pool the renewal is committed and the error is returned with it.
	g := newACMEFixture(t)
	g.seedPlatform(t)
	settingsExec(t, g.db, `update installation_settings set default_resolver_pool = null`)
	g.seedActive(t, certIDA, "a.example.test", "letsencrypt", "http-01", "staging", renewDue, nil)
	got, err = g.issuer.RenewLetsEncryptCertificate(context.Background(), g.due(t, certIDA), renewOptions())
	if err == nil || err.Error() != "certificate renewed but apply enqueue failed: "+reasonNoPool || got.Applied || got.Certificate.ID != certIDA {
		t.Fatalf("enqueue failure: %v", err)
	}
	if row := g.row(t, certIDA); row.pem == renewOldPEM {
		t.Fatal("the renewal must stay committed when the enqueue fails")
	}
}

func TestSQLiteRenewLetsEncryptDNS01UsesStoredCredentials(t *testing.T) {
	f := newACMEFixture(t)
	f.seedActive(t, certIDA, "d.example.test", "letsencrypt", "dns-01", "staging", renewDue, nil)
	cert := f.due(t, certIDA)

	// No credentials: failure_reason is recorded and ACME never runs.
	_, err := f.issuer.RenewLetsEncryptCertificate(context.Background(), cert, renewOptions())
	if err == nil || err.Error() != renewReasonDNS || f.fake.count() != 0 {
		t.Fatalf("missing credentials: %v", err)
	}
	row := f.row(t, certIDA)
	if row.failure != renewReasonDNS || row.pem != renewOldPEM {
		t.Fatal("only failure_reason may change when credentials are missing")
	}

	secret, _ := f.secrets.Put(context.Background(), connPurpose, cfToken)
	settingsExec(t, f.db, `insert into provider_connections (id,provider,name,secret_id,scope,enabled,created_at,updated_at)
		values ('dddddddd-0000-4000-8000-0000000000d1','cloudflare','n',?,'dns-01',1,'0','0')`, secret)
	if _, err := f.issuer.RenewLetsEncryptCertificate(context.Background(), cert, renewOptions()); err != nil {
		t.Fatal(err)
	}
	o := f.fake.last(t)
	if o.PropagationSeconds != 60 || o.Dns01 == nil || o.Http01 != nil || o.Challenge != "dns-01" {
		t.Fatal("dns-01 renewal must use 60s propagation and the Cloudflare adapter")
	}
	if row := f.row(t, certIDA); row.failure != "" || row.pem == renewOldPEM {
		t.Fatal("a successful renewal clears the failure and updates the certificate")
	}
}

func TestSQLiteRenewLetsEncryptACMEFailureOnlyMarksFailureReason(t *testing.T) {
	f := newACMEFixture(t)
	f.fake.fn = func(context.Context, acme.LetsEncryptOptions) (acme.Material, error) {
		return acme.Material{}, errors.New("acme exploded")
	}
	oldSecret := f.seedActive(t, certIDA, "a.example.test", "letsencrypt", "http-01", "staging", renewDue, nil)
	before := f.row(t, certIDA)
	got, err := f.issuer.RenewLetsEncryptCertificate(context.Background(), f.due(t, certIDA), renewOptions())
	if err == nil || err.Error() != "acme exploded" || !reflect.DeepEqual(got, RenewResult{}) {
		t.Fatalf("acme failure: %v", err)
	}
	after := f.row(t, certIDA)
	if after.failure != "acme exploded" || after.updated != *certKey(t, 123, 456) {
		t.Fatal("failure_reason and updated_at must be recorded")
	}
	after.failure, after.updated = before.failure, before.updated
	if !reflect.DeepEqual(before, after) || after.keySecret != oldSecret {
		t.Fatal("an ACME failure must leave the active row otherwise unchanged")
	}
	if _, keys, _ := f.rows(t); keys != 1 {
		t.Fatal("no new key secret on failure")
	}
}

func TestSQLiteRenewLetsEncryptGuardsWriteNothing(t *testing.T) {
	f := newACMEFixture(t)
	f.seedActive(t, certIDA, "a.example.test", "letsencrypt", "http-01", "staging", renewDue, nil)
	base := f.due(t, certIDA)
	edit := func(fn func(*domain.CertificateStatus)) domain.CertificateStatus { c := base; fn(&c); return c }
	for name, test := range map[string]struct {
		cert domain.CertificateStatus
		opts RenewalOptions
		want string
	}{
		"issuer":    {edit(func(c *domain.CertificateStatus) { c.Issuer = "self-signed" }), renewOptions(), "automatic renewal is only supported for Let's Encrypt certificates"},
		"inactive":  {edit(func(c *domain.CertificateStatus) { c.Status = "failed" }), renewOptions(), "certificate " + certIDA + " is not active"},
		"challenge": {edit(func(c *domain.CertificateStatus) { c.Challenge = "none" }), renewOptions(), `certificate ` + certIDA + ` has unsupported challenge "none"`},
		"directory": {base, RenewalOptions{ProductionDirectoryURL: renewProduction}, "ACME directory URL is required for renewal"},
	} {
		if _, err := f.issuer.RenewLetsEncryptCertificate(context.Background(), test.cert, test.opts); err == nil || err.Error() != test.want {
			t.Errorf("%s: %v", name, err)
		}
	}
	noKey := NewSQLiteCertificateIssueStore(f.db, settingsWriter(f.db), nil)
	noKey.issueACME = f.fake.issue
	if _, err := noKey.RenewLetsEncryptCertificate(context.Background(), base, renewOptions()); err == nil || err.Error() != "Let's Encrypt renewal requires a master key" {
		t.Errorf("master key: %v", err)
	}
	if f.fake.count() != 0 || f.row(t, certIDA).failure != "" {
		t.Fatal("guards must not call ACME nor write failure_reason")
	}
}

func TestSQLiteRenewLetsEncryptRowNoLongerActiveLeavesNoOrphanSecret(t *testing.T) {
	f := newACMEFixture(t)
	f.seedActive(t, certIDA, "a.example.test", "letsencrypt", "http-01", "staging", renewDue, nil)
	f.fake.fn = func(_ context.Context, o acme.LetsEncryptOptions) (acme.Material, error) {
		settingsExec(t, f.db, `update certificates set status = 'failed' where id = ?`, certIDA)
		return fakeLeaf(t, o.Hostnames, 90), nil
	}
	_, err := f.issuer.RenewLetsEncryptCertificate(context.Background(), f.due(t, certIDA), renewOptions())
	if err == nil || err.Error() != "certificate "+certIDA+" is no longer active" {
		t.Fatalf("no longer active: %v", err)
	}
	if _, keys, _ := f.rows(t); keys != 1 {
		t.Fatalf("the new key secret must roll back with the missed update, got %d key secrets", keys)
	}
	if row := f.row(t, certIDA); row.pem != renewOldPEM || row.failure != "" {
		t.Fatal("a row that is no longer active must stay untouched")
	}
}

func TestSQLiteRenewLetsEncryptSecretFailureMarksFailure(t *testing.T) {
	f := newACMEFixture(t)
	f.issuer = NewSQLiteCertificateIssueStore(f.db, settingsWriter(f.db), failPurposeSecrets{SQLiteStore: f.secrets, purpose: keyPurpose, err: errors.New("secret store down")})
	f.useFake()
	f.seedActive(t, certIDA, "a.example.test", "letsencrypt", "http-01", "staging", renewDue, nil)
	if _, err := f.issuer.RenewLetsEncryptCertificate(context.Background(), f.due(t, certIDA), renewOptions()); err == nil || err.Error() != "secret store down" {
		t.Fatalf("secret failure: %v", err)
	}
	if row := f.row(t, certIDA); row.failure != "secret store down" || row.pem != renewOldPEM {
		t.Fatal("secret failure records failure_reason and nothing else")
	}
}

func TestSQLiteRenewLetsEncryptNeverCallsACMEWhileAWriteIsHeld(t *testing.T) {
	f := newACMEFixture(t)
	f.seedActive(t, certIDA, "a.example.test", "letsencrypt", "http-01", "staging", renewDue, nil)
	var probeErr error
	f.fake.fn = func(ctx context.Context, o acme.LetsEncryptOptions) (acme.Material, error) {
		wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		probeErr = f.issuer.writer.Write(wctx, func(v sqlitedb.WriteValue) error {
			_, err := v.Conn().ExecContext(v.Context(), `update installation_settings set updated_at = updated_at`)
			return err
		})
		return fakeLeaf(t, o.Hostnames, 90), nil
	}
	if _, err := f.issuer.RenewLetsEncryptCertificate(context.Background(), f.due(t, certIDA), renewOptions()); err != nil || probeErr != nil {
		t.Fatalf("the writer must be free during renewal ACME: %v / %v", err, probeErr)
	}
}

func TestSQLiteRenewLetsEncryptCanceledDuringACMELeavesRowUntouched(t *testing.T) {
	f := newACMEFixture(t)
	f.seedActive(t, certIDA, "a.example.test", "letsencrypt", "http-01", "staging", renewDue, nil)
	ctx, cancel := context.WithCancel(context.Background())
	f.fake.fn = func(ctx context.Context, _ acme.LetsEncryptOptions) (acme.Material, error) {
		cancel()
		return acme.Material{}, ctx.Err()
	}
	_, err := f.issuer.RenewLetsEncryptCertificate(ctx, f.due(t, certIDA), renewOptions())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected the context error, got %v", err)
	}
	if row := f.row(t, certIDA); row.failure != "" || row.pem != renewOldPEM {
		t.Fatal("the best-effort failure mark cannot run on a canceled context")
	}
}

// ---- RenewDueCertificates ----

func newRenewFixture(t *testing.T) (*acmeFixture, *trackingLease, *domain.TopologyRole, *bytes.Buffer) {
	t.Helper()
	f := newACMEFixture(t)
	lease := &trackingLease{}
	role := domain.TopologyRoleStandalone
	logs := &bytes.Buffer{}
	return f, lease, &role, logs
}

func renewDueOptions(lease RenewalIdentityLease, role *domain.TopologyRole, logs *bytes.Buffer) RenewalOptions {
	opts := renewOptions()
	opts.Now = func() time.Time { return renewNow }
	opts.Identity = writableIdentity(role)
	opts.IdentityLease = lease
	opts.Log = log.New(logs, "", 0)
	return opts
}

func TestSQLiteRenewDueCertificatesMixedResultsMatchPG(t *testing.T) {
	f, lease, role, logs := newRenewFixture(t)
	f.seedPlatform(t)
	f.seedActive(t, certIDA, "ok.example.test", "letsencrypt", "http-01", "production", renewDue, nil)
	f.seedActive(t, certIDB, "fail.example.test", "letsencrypt", "http-01", "staging", renewDue+10, nil)
	f.seedActive(t, certIDC, "ss.example.test", "self-signed", "none", "local", renewDue+20, nil)
	f.seedActive(t, certIDD, "dns.example.test", "letsencrypt", "dns-01", "staging", renewDue+30, nil)
	f.seedActive(t, "cccccccc-0000-4000-8000-0000000000e1", "later.example.test", "letsencrypt", "http-01", "staging", renewFut, nil)
	f.seedActive(t, "cccccccc-0000-4000-8000-0000000000e2", "up.example.test", "uploaded", "none", "staging", renewDue, nil)
	settingsExec(t, f.db, `update certificates set status = 'failed' where id = ?`, "cccccccc-0000-4000-8000-0000000000e2")
	f.seedActive(t, "cccccccc-0000-4000-8000-0000000000e3", "off.example.test", "letsencrypt", "http-01", "staging", renewDue, nil)
	settingsExec(t, f.db, `update certificates set status = 'failed' where id = ?`, "cccccccc-0000-4000-8000-0000000000e3")
	f.fake.fn = func(_ context.Context, o acme.LetsEncryptOptions) (acme.Material, error) {
		if !lease.isHeld() {
			t.Error("ACME ran outside the identity lease")
		}
		if strings.HasPrefix(o.Hostnames[0], "fail") {
			return acme.Material{}, errors.New("acme exploded")
		}
		return fakeLeaf(t, o.Hostnames, 90), nil
	}

	renewed, failed, err := f.issuer.RenewDueCertificates(context.Background(), renewDueOptions(lease, role, logs))
	if err != nil || renewed != 2 || failed != 2 {
		t.Fatalf("renewed=%d failed=%d err=%v", renewed, failed, err)
	}
	if lease.calls != 4 || f.fake.count() != 2 {
		t.Fatalf("lease calls=%d acme calls=%d; only due active LE/self-signed rows are worked", lease.calls, f.fake.count())
	}
	for _, id := range []string{certIDA, certIDC} {
		if row := f.row(t, id); row.pem == renewOldPEM || row.failure != "" {
			t.Fatalf("%s was not renewed", id)
		}
	}
	if row := f.row(t, certIDB); row.pem != renewOldPEM || row.failure != "acme exploded" {
		t.Fatal("failed ACME renewal must only mark failure_reason")
	}
	if row := f.row(t, certIDD); row.pem != renewOldPEM || row.failure != renewReasonDNS {
		t.Fatal("missing credentials must only mark failure_reason")
	}
	if row := f.row(t, "cccccccc-0000-4000-8000-0000000000e1"); row.pem != renewOldPEM || row.failure != "" {
		t.Fatal("a certificate that is not due must be untouched")
	}
	if f.count(t, `select count(*) from apply_jobs`) != 2 {
		t.Fatal("each renewed certificate queues one apply job")
	}
	out := logs.String()
	for _, want := range []string{"acme exploded", "renewed and apply queued", renewReasonDNS} {
		if !strings.Contains(out, want) {
			t.Fatalf("log is missing %q", want)
		}
	}
}

func TestSQLiteRenewDueCertificatesProcessesInRenewAfterOrder(t *testing.T) {
	f, lease, role, logs := newRenewFixture(t)
	f.seedActive(t, certIDA, "third.example.test", "letsencrypt", "http-01", "staging", renewDue+300, nil)
	f.seedActive(t, certIDB, "first.example.test", "letsencrypt", "http-01", "staging", renewDue+100, nil)
	f.seedActive(t, certIDC, "second.example.test", "letsencrypt", "http-01", "staging", renewDue+200, nil)
	if renewed, failed, err := f.issuer.RenewDueCertificates(context.Background(), renewDueOptions(lease, role, logs)); err != nil || renewed != 3 || failed != 0 {
		t.Fatalf("renewed=%d failed=%d err=%v", renewed, failed, err)
	}
	var order []string
	for _, c := range f.fake.calls {
		order = append(order, c.Hostnames[0])
	}
	if !reflect.DeepEqual(order, []string{"first.example.test", "second.example.test", "third.example.test"}) {
		t.Fatalf("renewal order = %v", order)
	}
}

func TestSQLiteRenewDueCertificatesCountsCommittedRenewalWithEnqueueFailureAsFailed(t *testing.T) {
	// PG quirk (observed): the row is renewed, but the returned enqueue error
	// makes the item count as failed.
	f, lease, role, logs := newRenewFixture(t)
	f.seedPlatform(t)
	settingsExec(t, f.db, `update installation_settings set default_resolver_pool = null`)
	f.seedActive(t, certIDC, "ss.example.test", "self-signed", "none", "local", renewDue, nil)
	renewed, failed, err := f.issuer.RenewDueCertificates(context.Background(), renewDueOptions(lease, role, logs))
	if err != nil || renewed != 0 || failed != 1 {
		t.Fatalf("renewed=%d failed=%d err=%v", renewed, failed, err)
	}
	if row := f.row(t, certIDC); row.pem == renewOldPEM {
		t.Fatal("the row must be renewed even though the item counts as failed")
	}
	if !strings.Contains(logs.String(), "certificate renewed but apply enqueue failed") {
		t.Fatal("the enqueue failure must be logged")
	}
}

func TestSQLiteRenewDueCertificatesSkipsWithoutLeaseOrPolicy(t *testing.T) {
	f, lease, role, logs := newRenewFixture(t)
	f.seedActive(t, certIDA, "a.example.test", "letsencrypt", "http-01", "staging", renewDue, nil)
	node := domain.TopologyRoleNode
	for name, opts := range map[string]RenewalOptions{
		"no lease":     renewDueOptions(nil, role, logs),
		"nil identity": func() RenewalOptions { o := renewDueOptions(lease, role, logs); o.Identity = nil; return o }(),
		"node role":    renewDueOptions(lease, &node, logs),
		"unloaded": func() RenewalOptions {
			o := renewDueOptions(lease, role, logs)
			o.Identity = func(context.Context) (identity.Identity, bool, error) { return identity.Identity{}, false, nil }
			return o
		}(),
		"identity errors": func() RenewalOptions {
			o := renewDueOptions(lease, role, logs)
			o.Identity = func(context.Context) (identity.Identity, bool, error) {
				return identity.Identity{}, true, errors.New("down")
			}
			return o
		}(),
	} {
		if renewed, failed, err := f.issuer.RenewDueCertificates(context.Background(), opts); err != nil || renewed != 0 || failed != 0 {
			t.Errorf("%s: renewed=%d failed=%d err=%v", name, renewed, failed, err)
		}
	}
	if f.fake.count() != 0 || lease.calls != 0 || f.row(t, certIDA).pem != renewOldPEM {
		t.Fatal("nothing may be issued without a lease and an allowing policy")
	}
}

func TestSQLiteRenewDueCertificatesNothingDueAndNowSeam(t *testing.T) {
	f, lease, role, logs := newRenewFixture(t)
	if renewed, failed, err := f.issuer.RenewDueCertificates(context.Background(), renewDueOptions(lease, role, logs)); err != nil || renewed != 0 || failed != 0 || lease.calls != 0 {
		t.Fatal("an empty database has nothing due")
	}
	f.seedActive(t, certIDA, "a.example.test", "letsencrypt", "http-01", "staging", renewDue, nil)
	early := renewDueOptions(lease, role, logs)
	early.Now = func() time.Time { return time.Unix(renewDue-1, 0) }
	if renewed, failed, err := f.issuer.RenewDueCertificates(context.Background(), early); err != nil || renewed != 0 || failed != 0 {
		t.Fatal("Now must decide what is due")
	}
	exact := renewDueOptions(lease, role, logs)
	exact.Now = func() time.Time { return time.Unix(renewDue, 0) }
	if renewed, _, err := f.issuer.RenewDueCertificates(context.Background(), exact); err != nil || renewed != 1 {
		t.Fatalf("renew_after == now is due: renewed=%d err=%v", renewed, err)
	}
}

func TestSQLiteRenewDueCertificatesRechecksPolicyBeforeEveryItem(t *testing.T) {
	f, lease, role, logs := newRenewFixture(t)
	f.seedActive(t, certIDA, "first.example.test", "letsencrypt", "http-01", "staging", renewDue, nil)
	f.seedActive(t, certIDB, "second.example.test", "letsencrypt", "http-01", "staging", renewDue+10, nil)
	f.fake.fn = func(_ context.Context, o acme.LetsEncryptOptions) (acme.Material, error) {
		*role = domain.TopologyRoleNode // demoted while the first item runs
		return fakeLeaf(t, o.Hostnames, 90), nil
	}
	renewed, failed, err := f.issuer.RenewDueCertificates(context.Background(), renewDueOptions(lease, role, logs))
	if err != nil || renewed != 1 || failed != 0 || f.fake.count() != 1 || lease.calls != 1 {
		t.Fatalf("renewed=%d failed=%d acme=%d lease=%d err=%v", renewed, failed, f.fake.count(), lease.calls, err)
	}
	if f.row(t, certIDB).pem != renewOldPEM {
		t.Fatal("the demoted node must not renew the second item")
	}
}

func TestSQLiteRenewDueCertificatesLeaseErrors(t *testing.T) {
	for name, test := range map[string]struct {
		leaseErr       error
		renewed, fails int
		attempts       int
	}{
		"not permitted stops the sweep":                {identity.ErrAutomaticRenewalNotPermitted, 0, 0, 1},
		"identity not loaded stops":                    {identity.ErrIdentityNotLoaded, 0, 0, 1},
		"other lease error is a failure and continues": {errors.New("lease broke"), 0, 2, 2},
	} {
		t.Run(name, func(t *testing.T) {
			f, lease, role, logs := newRenewFixture(t)
			f.seedActive(t, certIDA, "a.example.test", "letsencrypt", "http-01", "staging", renewDue, nil)
			f.seedActive(t, certIDB, "b.example.test", "letsencrypt", "http-01", "staging", renewDue+10, nil)
			lease.before = func(context.Context) error { return test.leaseErr }
			renewed, failed, err := f.issuer.RenewDueCertificates(context.Background(), renewDueOptions(lease, role, logs))
			if err != nil || renewed != test.renewed || failed != test.fails || lease.calls != test.attempts || f.fake.count() != 0 {
				t.Fatalf("renewed=%d failed=%d lease=%d acme=%d err=%v", renewed, failed, lease.calls, f.fake.count(), err)
			}
		})
	}
}

func TestSQLiteRenewDueCertificatesCanceledContext(t *testing.T) {
	f, lease, role, logs := newRenewFixture(t)
	f.seedActive(t, certIDA, "a.example.test", "letsencrypt", "http-01", "staging", renewDue, nil)
	f.seedActive(t, certIDB, "b.example.test", "letsencrypt", "http-01", "staging", renewDue+10, nil)
	done, cancelDone := context.WithCancel(context.Background())
	cancelDone()
	if renewed, failed, err := f.issuer.RenewDueCertificates(done, renewDueOptions(lease, role, logs)); err != nil || renewed != 0 || failed != 0 || f.fake.count() != 0 {
		t.Fatalf("canceled before start: %d %d %v", renewed, failed, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	f.fake.fn = func(ctx context.Context, _ acme.LetsEncryptOptions) (acme.Material, error) {
		cancel()
		return acme.Material{}, ctx.Err()
	}
	renewed, failed, err := f.issuer.RenewDueCertificates(ctx, renewDueOptions(lease, role, logs))
	if err != nil || renewed != 0 || failed != 0 || f.fake.count() != 1 {
		t.Fatalf("canceled during ACME stops the sweep uncounted: %d %d acme=%d err=%v", renewed, failed, f.fake.count(), err)
	}
}

func TestSQLiteRenewDueCertificatesSelfSignedWithoutMasterKeyFails(t *testing.T) {
	f, lease, role, logs := newRenewFixture(t)
	f.seedActive(t, certIDC, "ss.example.test", "self-signed", "none", "local", renewDue, nil)
	f.issuer = NewSQLiteCertificateIssueStore(f.db, settingsWriter(f.db), nil)
	renewed, failed, err := f.issuer.RenewDueCertificates(context.Background(), renewDueOptions(lease, role, logs))
	if err != nil || renewed != 0 || failed != 1 || !strings.Contains(logs.String(), "self-signed renewal requires a master key") {
		t.Fatalf("renewed=%d failed=%d err=%v", renewed, failed, err)
	}
}

func TestSQLiteRenewDueCertificatesListErrorIsReturned(t *testing.T) {
	f, lease, role, logs := newRenewFixture(t)
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	renewed, failed, err := f.issuer.RenewDueCertificates(context.Background(), renewDueOptions(lease, role, logs))
	if err == nil || renewed != 0 || failed != 0 {
		t.Fatalf("a due-list error must be returned: %d %d %v", renewed, failed, err)
	}
}

func TestSQLiteRenewDueCertificatesInfinitySentinelFailsTheListLikePG(t *testing.T) {
	// renew_after '0' is -infinity: PG's due list errors on it (see the 56a
	// sentinel tests), so the sweep returns the error and renews nothing.
	f, lease, role, logs := newRenewFixture(t)
	f.seedActive(t, certIDA, "a.example.test", "letsencrypt", "http-01", "staging", renewDue, nil)
	settingsExec(t, f.db, `update certificates set renew_after = '0'`)
	renewed, failed, err := f.issuer.RenewDueCertificates(context.Background(), renewDueOptions(lease, role, logs))
	if err == nil || renewed != 0 || failed != 0 || f.fake.count() != 0 {
		t.Fatalf("renewed=%d failed=%d err=%v", renewed, failed, err)
	}
}
