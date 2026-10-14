package configuration_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/cluster"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
	nodesync "github.com/zerkc/ProxyCore/apps/api/internal/sync"
)

var _ configuration.PrimaryGrantStore = (*configuration.SQLitePhase2Store)(nil)

var errGrantRollback = errors.New("grant rollback sentinel")

type grantFixture struct {
	t         *testing.T
	db        *sql.DB
	store     *configuration.SQLitePhase2Store
	now       time.Time
	primaryID string
	issued    atomic.Int32
	service   *enrollment.PrimaryService
	owner     string
}

func grantStamp(t *testing.T, at time.Time) string {
	t.Helper()
	key, err := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: at.Unix(), Nanos: int32(at.Nanosecond())})
	if err != nil {
		t.Fatal(err)
	}
	return *key
}

func newGrantFixture(t *testing.T) *grantFixture {
	t.Helper()
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "grant.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := sqlitedb.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	writer := sqlitedb.NewWriter(db, nil)
	master := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x37}, 32))
	keys := cluster.NewSQLiteKeyStore(db, writer, master)
	f := &grantFixture{
		t: t, db: db, now: time.Date(2026, 10, 2, 3, 4, 5, 0, time.UTC),
		primaryID: uuid.NewString(), owner: uuid.NewString(),
		store: configuration.NewSQLitePhase2Store(db, writer, keys, master),
	}
	f.exec(`insert into users(id,username,password_hash,role,created_at,updated_at) values(?,'grant-owner','dummy','owner','0','0')`, f.owner)
	f.exec(`insert into installation_identity(id,installation_id,node_id,role,leadership_generation,latest_known_generation,updated_at)
		values('default',?,?,'primary',7,7,'0')`, f.primaryID, uuid.NewString())
	f.service = f.newService(func() time.Time { return f.now }, nil)
	return f
}

func (f *grantFixture) newService(now func() time.Time, seal enrollment.SealBootstrapFunc) *enrollment.PrimaryService {
	return enrollment.NewPrimaryService(f.store, enrollment.PrimaryServiceOptions{
		Now:           now,
		SealBootstrap: seal,
		CredentialIssuer: enrollment.NodeCredentialIssuerFunc{
			Issue: func(context.Context) (enrollment.NodeCredentialMaterial, error) {
				f.issued.Add(1)
				secret := make([]byte, nodesync.NodeCredentialSecretBytes)
				if _, err := rand.Read(secret); err != nil {
					return enrollment.NodeCredentialMaterial{}, err
				}
				credential, err := nodesync.NewNodeCredential(uuid.NewString(), secret)
				if err != nil {
					return enrollment.NodeCredentialMaterial{}, err
				}
				defer credential.Destroy()
				return enrollment.NodeCredentialMaterial{
					ID: credential.ID(), Secret: secret,
					Hash: credential.Hash(), HashVersion: nodesync.NodeCredentialHashVersion,
				}, nil
			},
			Validate: func(enrollment.NodeCredentialMaterial) error { return nil },
		},
	})
}

func (f *grantFixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(query, args...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *grantFixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow(query, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// insertToken stores a token matching the enrollment hashing contract and
// returns its plaintext form plus row id.
func (f *grantFixture) insertToken(selectorByte string, secretByte byte, expires time.Time) (plaintext, id string) {
	f.t.Helper()
	secret := bytes.Repeat([]byte{secretByte}, 32)
	sum := sha256.Sum256(append([]byte("proxycore/enrollment-token/"), secret...))
	id = uuid.NewString()
	selector := strings.Repeat(selectorByte, 16)
	f.exec(`insert into enrollment_tokens(id,token_selector,token_hash,hash_version,created_by_user_id,created_at,expires_at)
		values(?,?,?,?,?,?,?)`, id, selector, hex.EncodeToString(sum[:]), configuration.EnrollmentTokenHashVersion, f.owner,
		grantStamp(f.t, f.now.Add(-time.Hour)), grantStamp(f.t, expires))
	return "pcenr1_" + selector + "_" + base64.RawURLEncoding.EncodeToString(secret), id
}

type grantRecipient struct {
	key enrollment.BootstrapRecipientPublicKey
}

func newRecipient(t *testing.T) grantRecipient {
	t.Helper()
	r, err := enrollment.NewBootstrapRecipient()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Destroy)
	key, err := r.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	return grantRecipient{key: key}
}

func (f *grantFixture) request(token string, recipient grantRecipient) enrollment.PrimaryGrantRequest {
	return enrollment.PrimaryGrantRequest{
		EnrollmentToken:               token,
		AttemptID:                     "11112222-3333-4444-8999-aabbccddeeff",
		TargetInstallationID:          "33334444-5555-4666-8999-aabbccddeeff",
		TargetNodeID:                  "44445555-6666-4777-8999-aabbccddeeff",
		RecipientPublicKey:            recipient.key,
		VerifiedPreviewDigest:         bytes.Repeat([]byte{0x29}, sha256.Size),
		ExpectedPrimaryInstallationID: f.primaryID,
		ExpectedLeadershipGeneration:  7,
		GrantTTL:                      20 * time.Minute,
	}
}

func (f *grantFixture) assertNoGrantRows() {
	f.t.Helper()
	for _, table := range []string{"node_credentials", "enrolled_nodes", "enrollment_grants"} {
		if n := f.count(`select count(*) from ` + table); n != 0 {
			f.t.Fatalf("%s rows = %d, want 0", table, n)
		}
	}
}

func (f *grantFixture) role() string {
	f.t.Helper()
	var role string
	if err := f.db.QueryRow(`select role from installation_identity where id='default'`).Scan(&role); err != nil {
		f.t.Fatal(err)
	}
	return role
}

func TestSQLitePrimaryGrantHappyPath(t *testing.T) {
	f := newGrantFixture(t)
	token, tokenID := f.insertToken("a1", 0x41, f.now.Add(time.Hour))
	request := f.request(token, newRecipient(t))
	response, err := f.service.IssueGrant(context.Background(), request)
	if err != nil {
		t.Fatalf("IssueGrant: %v", err)
	}
	if len(response.SealedBootstrapPayload) == 0 || response.PayloadHash == "" || !response.ExpiresAt.Equal(f.now.Add(20*time.Minute)) {
		t.Fatalf("response = %+v", response)
	}
	if f.role() != "primary-with-nodes" {
		t.Fatalf("role = %s", f.role())
	}
	for table, want := range map[string]int{"node_credentials": 1, "enrolled_nodes": 1, "enrollment_grants": 1} {
		if n := f.count(`select count(*) from ` + table); n != want {
			t.Fatalf("%s rows = %d", table, n)
		}
	}
	if n := f.count(`select count(*) from enrollment_tokens where id=? and consumed_at is not null and consumed_by_attempt_id=?`, tokenID, request.AttemptID); n != 1 {
		t.Fatal("token not consumed by attempt")
	}
	if n := f.count(`select count(*) from installation_identity where cluster_key_id is not null`); n != 1 {
		t.Fatal("cluster key not bound")
	}
	// Only hashes are persisted: neither the token secret nor a node bearer
	// appears in any grant record column.
	var stored string
	if err := f.db.QueryRow(`select credential_hash from node_credentials`).Scan(&stored); err != nil || len(stored) != 64 {
		t.Fatalf("credential hash = %q err=%v", stored, err)
	}
	if n := f.count(`select count(*) from enrollment_tokens where token_hash = ?`, token); n != 0 {
		t.Fatal("plaintext token persisted")
	}
}

func TestSQLitePrimaryGrantConcurrentSameToken(t *testing.T) {
	f := newGrantFixture(t)
	token, _ := f.insertToken("a1", 0x41, f.now.Add(time.Hour))
	request := f.request(token, newRecipient(t))
	const workers = 8
	payloads := make([][]byte, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			response, err := f.service.IssueGrant(context.Background(), request)
			payloads[i], errs[i] = response.SealedBootstrapPayload, err
		}(i)
	}
	close(start)
	wg.Wait()
	for i := range errs {
		if errs[i] != nil || !bytes.Equal(payloads[i], payloads[0]) {
			t.Fatalf("worker %d err=%v identical=%v", i, errs[i], bytes.Equal(payloads[i], payloads[0]))
		}
	}
	if f.issued.Load() != 1 || f.count(`select count(*) from enrollment_grants`) != 1 || f.count(`select count(*) from node_credentials`) != 1 {
		t.Fatalf("issued=%d, want exactly one grant and credential", f.issued.Load())
	}
}

func TestSQLitePrimaryGrantConcurrentDifferentBindings(t *testing.T) {
	f := newGrantFixture(t)
	token, _ := f.insertToken("a1", 0x41, f.now.Add(time.Hour))
	recipient := newRecipient(t)
	first := f.request(token, recipient)
	second := f.request(token, recipient)
	second.AttemptID = "99998888-3333-4444-8999-aabbccddeeff"
	second.TargetInstallationID = "99998888-5555-4666-8999-aabbccddeeff"
	second.TargetNodeID = "99998888-6666-4777-8999-aabbccddeeff"
	var wg sync.WaitGroup
	results := make([]error, 2)
	for i, request := range []enrollment.PrimaryGrantRequest{first, second} {
		wg.Add(1)
		go func(i int, request enrollment.PrimaryGrantRequest) {
			defer wg.Done()
			_, results[i] = f.service.IssueGrant(context.Background(), request)
		}(i, request)
	}
	wg.Wait()
	succeeded := 0
	for _, err := range results {
		if err == nil {
			succeeded++
		} else if !errors.Is(err, enrollment.ErrEnrollmentDenied) {
			t.Fatalf("unexpected error %v", err)
		}
	}
	if succeeded != 1 || f.count(`select count(*) from enrollment_grants`) != 1 || f.count(`select count(*) from enrolled_nodes`) != 1 {
		t.Fatalf("succeeded=%d, want exactly one grant", succeeded)
	}
}

func TestSQLitePrimaryGrantExactRetryReturnsSamePayloadUntilExpiry(t *testing.T) {
	f := newGrantFixture(t)
	token, _ := f.insertToken("a1", 0x41, f.now.Add(time.Hour))
	request := f.request(token, newRecipient(t))
	first, err := f.service.IssueGrant(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(5 * time.Minute)
	retry, err := f.service.IssueGrant(context.Background(), request)
	if err != nil || !bytes.Equal(retry.SealedBootstrapPayload, first.SealedBootstrapPayload) || retry.PayloadHash != first.PayloadHash {
		t.Fatalf("retry err=%v same=%v", err, bytes.Equal(retry.SealedBootstrapPayload, first.SealedBootstrapPayload))
	}
	if f.issued.Load() != 1 || f.count(`select count(*) from node_credentials`) != 1 {
		t.Fatal("retry created a second credential")
	}
	f.now = f.now.Add(time.Hour)
	if _, err := f.service.IssueGrant(context.Background(), request); !errors.Is(err, enrollment.ErrEnrollmentDenied) {
		t.Fatalf("retry after grant expiry err = %v", err)
	}
}

func TestSQLitePrimaryGrantDifferentBindingAfterConsumeDenied(t *testing.T) {
	f := newGrantFixture(t)
	token, _ := f.insertToken("a1", 0x41, f.now.Add(time.Hour))
	recipient := newRecipient(t)
	request := f.request(token, recipient)
	if _, err := f.service.IssueGrant(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	other := newRecipient(t)
	mutations := map[string]func(*enrollment.PrimaryGrantRequest){
		"ephemeral key": func(r *enrollment.PrimaryGrantRequest) { r.RecipientPublicKey = other.key },
		"node":          func(r *enrollment.PrimaryGrantRequest) { r.TargetNodeID = "55556666-6666-4777-8999-aabbccddeeff" },
		"installation": func(r *enrollment.PrimaryGrantRequest) {
			r.TargetInstallationID = "55556666-5555-4666-8999-aabbccddeeff"
		},
		"attempt": func(r *enrollment.PrimaryGrantRequest) { r.AttemptID = "55556666-3333-4444-8999-aabbccddeeff" },
		"preview": func(r *enrollment.PrimaryGrantRequest) {
			r.VerifiedPreviewDigest = bytes.Repeat([]byte{0x30}, sha256.Size)
		},
	}
	for name, mutate := range mutations {
		changed := request
		mutate(&changed)
		if _, err := f.service.IssueGrant(context.Background(), changed); !errors.Is(err, enrollment.ErrEnrollmentDenied) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	if f.count(`select count(*) from enrollment_grants`) != 1 || f.count(`select count(*) from node_credentials`) != 1 ||
		f.count(`select count(*) from enrolled_nodes`) != 1 || f.issued.Load() != 1 {
		t.Fatal("denied bindings left extra rows")
	}
}

func TestSQLitePrimaryGrantExpiredOrRevokedTokenDenied(t *testing.T) {
	f := newGrantFixture(t)
	recipient := newRecipient(t)
	expired, _ := f.insertToken("b2", 0x42, f.now.Add(-time.Minute))
	revoked, revokedID := f.insertToken("c3", 0x43, f.now.Add(time.Hour))
	f.exec(`update enrollment_tokens set revoked_at = ? where id = ?`, grantStamp(t, f.now.Add(-time.Second)), revokedID)
	for name, token := range map[string]string{"expired": expired, "revoked": revoked} {
		if _, err := f.service.IssueGrant(context.Background(), f.request(token, recipient)); !errors.Is(err, enrollment.ErrEnrollmentDenied) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	f.assertNoGrantRows()
	if f.role() != "primary" || f.issued.Load() != 0 {
		t.Fatalf("role=%s issued=%d", f.role(), f.issued.Load())
	}
}

func TestSQLitePrimaryGrantRollsBackMidTransaction(t *testing.T) {
	f := newGrantFixture(t)
	_, tokenID := f.insertToken("a1", 0x41, f.now.Add(time.Hour))
	ctx := context.Background()
	attempt := "11112222-3333-4444-8999-aabbccddeeff"
	err := f.store.WithPrimaryGrant(ctx, func(tx configuration.PrimaryGrantTransaction) error {
		if err := tx.CreateNodeCredential(ctx, configuration.NodeCredentialRecord{
			ID: uuid.NewString(), NodeID: "44445555-6666-4777-8999-aabbccddeeff",
			CredentialHash: strings.Repeat("ab", 32), HashVersion: nodesync.NodeCredentialHashVersion, CreatedAt: f.now,
		}); err != nil {
			return err
		}
		if err := tx.ConsumeEnrollmentToken(ctx, tokenID, attempt, f.now); err != nil {
			return err
		}
		if err := tx.TransitionPrimaryToWithNodes(ctx, 7); err != nil {
			return err
		}
		return errGrantRollback
	})
	if !errors.Is(err, errGrantRollback) {
		t.Fatalf("err = %v", err)
	}
	f.assertNoGrantRows()
	if f.count(`select count(*) from enrollment_tokens where consumed_at is not null`) != 0 || f.role() != "primary" {
		t.Fatal("rollback left token consumed or role changed")
	}
	// A seal failure aborts the exchange before any write as well.
	token, _ := f.insertToken("d4", 0x44, f.now.Add(time.Hour))
	failing := f.newService(func() time.Time { return f.now }, func(enrollment.BootstrapRecipientPublicKey, enrollment.BootstrapBinding, enrollment.BootstrapGrant) ([]byte, error) {
		return nil, errGrantRollback
	})
	if _, err := failing.IssueGrant(ctx, f.request(token, newRecipient(t))); !errors.Is(err, enrollment.ErrEnrollmentDenied) {
		t.Fatalf("seal failure err = %v", err)
	}
	f.assertNoGrantRows()
}

func TestSQLitePrimaryGrantTransitionRequiresExactGeneration(t *testing.T) {
	f := newGrantFixture(t)
	ctx := context.Background()
	err := f.store.WithPrimaryGrant(ctx, func(tx configuration.PrimaryGrantTransaction) error {
		return tx.TransitionPrimaryToWithNodes(ctx, 6)
	})
	if !errors.Is(err, configuration.ErrEnrollmentGrantStore) || f.role() != "primary" {
		t.Fatalf("err=%v role=%s", err, f.role())
	}
}

// reenrollRequest builds a second enrollment for the same node, as happens in
// the switchover cycle where a former node returns to the same primary.
func (f *grantFixture) reenrollRequest(token string, attemptID string) enrollment.PrimaryGrantRequest {
	request := f.request(token, newRecipient(f.t))
	request.AttemptID = attemptID
	return request
}

func (f *grantFixture) revokeNodes() {
	f.t.Helper()
	// Same statements the demote/promote path runs when it retires a cluster.
	f.exec(`update node_credentials set revoked_at = ? where revoked_at is null`, grantStamp(f.t, f.now))
	f.exec(`update enrolled_nodes set revoked_at = ? where revoked_at is null`, grantStamp(f.t, f.now))
}

func TestSQLitePrimaryGrantRotatesRevokedNodeInPlace(t *testing.T) {
	f := newGrantFixture(t)
	ctx := context.Background()
	first, _ := f.insertToken("a1", 0x41, f.now.Add(time.Hour))
	if _, err := f.service.IssueGrant(ctx, f.request(first, newRecipient(t))); err != nil {
		t.Fatal(err)
	}
	var oldCredential string
	if err := f.db.QueryRow(`select credential_id from enrolled_nodes`).Scan(&oldCredential); err != nil {
		t.Fatal(err)
	}
	f.revokeNodes()
	f.now = f.now.Add(time.Minute)

	second, secondID := f.insertToken("b2", 0x42, f.now.Add(time.Hour))
	request := f.reenrollRequest(second, "66667777-3333-4444-8999-aabbccddeeff")
	response, err := f.service.IssueGrant(ctx, request)
	if err != nil {
		t.Fatalf("re-enrollment of a revoked node: %v", err)
	}
	if f.count(`select count(*) from enrolled_nodes where revoked_at is null`) != 1 ||
		f.count(`select count(*) from enrolled_nodes`) != 1 ||
		f.count(`select count(*) from node_credentials where revoked_at is null`) != 1 ||
		f.count(`select count(*) from node_credentials`) != 1 {
		t.Fatal("expected exactly one live enrolled row and one live credential row")
	}
	var newCredential, attempt string
	if err := f.db.QueryRow(`select credential_id, created_by_attempt_id from enrolled_nodes`).Scan(&newCredential, &attempt); err != nil {
		t.Fatal(err)
	}
	if newCredential == oldCredential || attempt != request.AttemptID {
		t.Fatalf("row not rotated: credential=%s attempt=%s", newCredential, attempt)
	}
	if f.count(`select count(*) from node_credentials where id = ?`, oldCredential) != 0 {
		t.Fatal("old credential still present")
	}
	if f.count(`select count(*) from enrollment_tokens where id=? and consumed_by_attempt_id=?`, secondID, request.AttemptID) != 1 {
		t.Fatal("token not consumed by the rotating attempt")
	}
	// Exact retry returns the same payload and does not rotate again.
	retry, err := f.service.IssueGrant(ctx, request)
	if err != nil || !bytes.Equal(retry.SealedBootstrapPayload, response.SealedBootstrapPayload) {
		t.Fatalf("retry err=%v", err)
	}
	var after string
	if err := f.db.QueryRow(`select credential_id from enrolled_nodes`).Scan(&after); err != nil || after != newCredential {
		t.Fatalf("retry rotated the credential: %v", err)
	}
	if f.issued.Load() != 2 {
		t.Fatalf("issued = %d, want 2", f.issued.Load())
	}
}

func TestSQLitePrimaryGrantRotationDeniedForLiveRowOrOtherInstallation(t *testing.T) {
	f := newGrantFixture(t)
	ctx := context.Background()
	first, _ := f.insertToken("a1", 0x41, f.now.Add(time.Hour))
	if _, err := f.service.IssueGrant(ctx, f.request(first, newRecipient(t))); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		var out string
		if err := f.db.QueryRow(`select n.node_id||n.installation_id||n.credential_id||n.created_by_attempt_id||ifnull(n.revoked_at,'live')||c.credential_hash||ifnull(c.revoked_at,'live')
			from enrolled_nodes n join node_credentials c on c.id = n.credential_id`).Scan(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := snapshot()

	// Live (unrevoked) row for the same node: denied, nothing changes.
	live, _ := f.insertToken("b2", 0x42, f.now.Add(time.Hour))
	if _, err := f.service.IssueGrant(ctx, f.reenrollRequest(live, "66667777-3333-4444-8999-aabbccddeeff")); !errors.Is(err, enrollment.ErrEnrollmentDenied) {
		t.Fatalf("live row err = %v", err)
	}
	if snapshot() != before || f.count(`select count(*) from enrollment_grants`) != 1 {
		t.Fatal("denied live-row exchange changed state")
	}

	// Revoked row but claimed by a different installation: denied.
	f.revokeNodes()
	revokedBefore := snapshot()
	other, _ := f.insertToken("c3", 0x43, f.now.Add(time.Hour))
	hijack := f.reenrollRequest(other, "77778888-3333-4444-8999-aabbccddeeff")
	hijack.TargetInstallationID = "99998888-5555-4666-8999-aabbccddeeff"
	if _, err := f.service.IssueGrant(ctx, hijack); !errors.Is(err, enrollment.ErrEnrollmentDenied) {
		t.Fatalf("other installation err = %v", err)
	}
	if snapshot() != revokedBefore || f.count(`select count(*) from enrollment_grants`) != 1 ||
		f.count(`select count(*) from enrollment_tokens where consumed_at is not null`) != 1 {
		t.Fatal("denied hijack changed state")
	}
}
