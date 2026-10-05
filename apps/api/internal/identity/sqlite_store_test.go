package identity

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

const (
	sqliteInstA = "aaaaaaaa-1111-4111-8111-111111111111"
	sqliteNodeA = "22222222-2222-4111-8111-222222222222"
)

type sqliteIdentityFixture struct {
	db    *sql.DB
	store *SQLiteStore
	// tick advances the injected writer clock by one second per Write.
	tick int64
}

func newSQLiteIdentityFixture(t *testing.T) *sqliteIdentityFixture {
	t.Helper()
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "identity.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := sqlitedb.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	f := &sqliteIdentityFixture{db: db, tick: 1_700_000_000}
	var mu sync.Mutex
	writer := sqlitedb.NewWriter(db, func() sqlitedb.FiniteTimestamp {
		mu.Lock()
		defer mu.Unlock()
		f.tick++
		return sqlitedb.FiniteTimestamp{Seconds: f.tick, Nanos: 123}
	})
	f.store = NewSQLiteStore(db, writer)
	return f
}

func (f *sqliteIdentityFixture) rawUpdatedAt(t *testing.T) string {
	t.Helper()
	var key string
	if err := f.db.QueryRow(`select updated_at from installation_identity where id='default'`).Scan(&key); err != nil {
		t.Fatal(err)
	}
	return key
}

func (f *sqliteIdentityFixture) rowCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(`select count(*) from installation_identity`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// identityStoreContract runs the fakeStore expectations from service_test.go
// against any Store, so a PG-backed factory can reuse it.
func identityStoreContract(t *testing.T, factory func(t *testing.T) Store) {
	ctx := context.Background()
	key := uuid.MustParse("cccccccc-1111-4111-8111-111111111111")
	t.Run("missing row is ErrNotFound everywhere", func(t *testing.T) {
		s := factory(t)
		if _, err := s.Get(ctx); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get: %v", err)
		}
		for name, err := range map[string]error{
			"role":   s.UpdateRole(ctx, domain.TopologyRolePrimary),
			"lead":   s.UpdateLeadershipGeneration(ctx, 3),
			"latest": s.UpdateLatestKnownGeneration(ctx, 3),
			"key":    s.UpdateClusterKeyID(ctx, nil),
		} {
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("%s: %v", name, err)
			}
		}
	})
	t.Run("invalid role is rejected before lookup", func(t *testing.T) {
		s := factory(t)
		if err := s.UpdateRole(ctx, "bogus"); err == nil || err.Error() != "invalid role: bogus" {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("ensure fresh then hit", func(t *testing.T) {
		s := factory(t)
		id, created, err := s.Ensure(ctx, sqliteInstA, sqliteNodeA)
		if err != nil || !created {
			t.Fatalf("fresh: %v created=%v", err, created)
		}
		if id.InstallationID != sqliteInstA || id.NodeID != sqliteNodeA || id.Role != domain.TopologyRoleStandalone ||
			id.LeadershipGeneration != 1 || id.LatestKnownGeneration != 1 || id.ClusterKeyID != nil || id.UpdatedAt.IsZero() {
			t.Fatalf("fresh identity: %+v", id)
		}
		again, created, err := s.Ensure(ctx, "11111111-1111-4111-8111-111111111111", "33333333-3333-4333-8333-333333333333")
		if err != nil || created || again.InstallationID != sqliteInstA || again.NodeID != sqliteNodeA {
			t.Fatalf("hit: %+v created=%v err=%v", again, created, err)
		}
		got, err := s.Get(ctx)
		if err != nil || got.InstallationID != sqliteInstA || got.Role != domain.TopologyRoleStandalone {
			t.Fatalf("get: %+v %v", got, err)
		}
	})
	t.Run("updates", func(t *testing.T) {
		s := factory(t)
		if _, _, err := s.Ensure(ctx, sqliteInstA, sqliteNodeA); err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateRole(ctx, domain.TopologyRoleNode); err != nil {
			t.Fatal(err)
		}
		// leadership generation: assigned verbatim, latest only grows.
		for _, step := range []struct{ set, lead, latest int64 }{{5, 5, 5}, {3, 3, 5}, {5, 5, 5}, {9, 9, 9}} {
			if err := s.UpdateLeadershipGeneration(ctx, domain.LeadershipGeneration(step.set)); err != nil {
				t.Fatal(err)
			}
			got, _ := s.Get(ctx)
			if int64(got.LeadershipGeneration) != step.lead || int64(got.LatestKnownGeneration) != step.latest {
				t.Fatalf("lead %d: %d/%d", step.set, got.LeadershipGeneration, got.LatestKnownGeneration)
			}
		}
		for _, step := range []struct{ set, latest int64 }{{4, 9}, {20, 20}, {7, 20}} {
			if err := s.UpdateLatestKnownGeneration(ctx, domain.LeadershipGeneration(step.set)); err != nil {
				t.Fatal(err)
			}
			got, _ := s.Get(ctx)
			if got.LeadershipGeneration != 9 || int64(got.LatestKnownGeneration) != step.latest {
				t.Fatalf("latest %d: %d/%d", step.set, got.LeadershipGeneration, got.LatestKnownGeneration)
			}
		}
		if err := s.UpdateClusterKeyID(ctx, &key); err != nil {
			t.Fatal(err)
		}
		got, _ := s.Get(ctx)
		if got.Role != domain.TopologyRoleNode || got.ClusterKeyID == nil || *got.ClusterKeyID != key {
			t.Fatalf("after updates: %+v", got)
		}
		if err := s.UpdateClusterKeyID(ctx, nil); err != nil {
			t.Fatal(err)
		}
		if got, _ = s.Get(ctx); got.ClusterKeyID != nil {
			t.Fatalf("cluster key not cleared: %v", got.ClusterKeyID)
		}
	})
}

func TestSQLiteStoreContract(t *testing.T) {
	identityStoreContract(t, func(t *testing.T) Store { return newSQLiteIdentityFixture(t).store })
}

func TestSQLiteStoreEnsureConvergesConcurrentFirstBoots(t *testing.T) {
	f := newSQLiteIdentityFixture(t)
	const n = 16
	type result struct {
		id      Identity
		created bool
		err     error
	}
	results := make([]result, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			id, created, err := f.store.Ensure(context.Background(), domain.InstallationID(uuid.NewString()), domain.NodeID(uuid.NewString()))
			results[i] = result{id, created, err}
		}()
	}
	close(start)
	wg.Wait()
	created := 0
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("caller %d: %v", i, r.err)
		}
		if r.created {
			created++
		}
		if r.id.InstallationID != results[0].id.InstallationID || r.id.NodeID != results[0].id.NodeID {
			t.Fatalf("caller %d diverged: %+v vs %+v", i, r.id, results[0].id)
		}
	}
	if created != 1 || f.rowCount(t) != 1 {
		t.Fatalf("created=%d rows=%d", created, f.rowCount(t))
	}
	got, err := f.store.Get(context.Background())
	if err != nil || got.InstallationID != results[0].id.InstallationID {
		t.Fatalf("winner not persisted: %+v %v", got, err)
	}
}

func TestSQLiteStoreEnsureValidatesUUIDsLikePG(t *testing.T) {
	f := newSQLiteIdentityFixture(t)
	ctx := context.Background()
	for _, c := range []struct {
		name, inst, node, want string
	}{
		{"bad installation", "nope", sqliteNodeA, "invalid installation id: invalid UUID length: 4"},
		{"bad node", sqliteInstA, "nope", "invalid node id: invalid UUID length: 4"},
		{"empty", "", "", "invalid installation id: invalid UUID length: 0"},
		{"installation checked first", "x", "y", "invalid installation id: invalid UUID length: 1"},
		{"unparsable hex", "zzzzzzzz-1111-4111-8111-111111111111", sqliteNodeA, "invalid installation id: invalid UUID format"},
	} {
		_, created, err := f.store.Ensure(ctx, domain.InstallationID(c.inst), domain.NodeID(c.node))
		if err == nil || err.Error() != c.want || created {
			t.Errorf("%s: err=%v created=%v", c.name, err, created)
		}
	}
	if f.rowCount(t) != 0 {
		t.Fatal("invalid input created a row")
	}
	// Like PG, validation happens only on a miss: a hit ignores the arguments.
	if _, _, err := f.store.Ensure(ctx, sqliteInstA, sqliteNodeA); err != nil {
		t.Fatal(err)
	}
	got, created, err := f.store.Ensure(ctx, "bad", "bad")
	if err != nil || created || got.InstallationID != sqliteInstA {
		t.Fatalf("hit with invalid input: %+v created=%v err=%v", got, created, err)
	}
}

func TestSQLiteStoreEnsureCanonicalizesUUIDs(t *testing.T) {
	f := newSQLiteIdentityFixture(t)
	id, created, err := f.store.Ensure(context.Background(),
		"AAAAAAAA-1111-4111-8111-111111111111", "{22222222-2222-4111-8111-222222222222}")
	if err != nil || !created {
		t.Fatalf("%v created=%v", err, created)
	}
	// Divergence from PG (which echoes the raw input on a miss): the returned
	// identity equals what a later Get returns.
	if id.InstallationID != sqliteInstA || id.NodeID != sqliteNodeA {
		t.Fatalf("not canonical: %+v", id)
	}
	var inst, node string
	if err := f.db.QueryRow(`select installation_id, node_id from installation_identity where id='default'`).Scan(&inst, &node); err != nil {
		t.Fatal(err)
	}
	if inst != sqliteInstA || node != sqliteNodeA {
		t.Fatalf("stored %q %q", inst, node)
	}
	got, _ := f.store.Get(context.Background())
	if got.InstallationID != id.InstallationID || got.NodeID != id.NodeID || !got.UpdatedAt.Equal(id.UpdatedAt) {
		t.Fatalf("Ensure %+v != Get %+v", id, got)
	}
}

func TestSQLiteStoreStoresSingletonDefaults(t *testing.T) {
	f := newSQLiteIdentityFixture(t)
	if _, _, err := f.store.Ensure(context.Background(), sqliteInstA, sqliteNodeA); err != nil {
		t.Fatal(err)
	}
	var id, role string
	var lead, latest int64
	var key sql.NullString
	if err := f.db.QueryRow(`select id, role, leadership_generation, latest_known_generation, cluster_key_id from installation_identity`).
		Scan(&id, &role, &lead, &latest, &key); err != nil {
		t.Fatal(err)
	}
	if id != "default" || role != "standalone-primary" || lead != 1 || latest != 1 || key.Valid {
		t.Fatalf("row: %q %q %d %d %v", id, role, lead, latest, key)
	}
}

func TestSQLiteStoreGenerationsUseSignedMaxLikePG(t *testing.T) {
	f := newSQLiteIdentityFixture(t)
	ctx := context.Background()
	if _, _, err := f.store.Ensure(ctx, sqliteInstA, sqliteNodeA); err != nil {
		t.Fatal(err)
	}
	// PG observation: bigint greatest() is signed, so a wrapped uint64 (-2 as
	// int64) is assigned to leadership but never raises latest_known.
	steps := []struct {
		call         string
		gen          domain.LeadershipGeneration
		lead, latest int64
	}{
		{"lead", 5, 5, 5},
		{"lead", 3, 3, 5},
		{"lead", 0, 0, 5},
		{"lead", domain.LeadershipGeneration(^uint64(1)), -2, 5},
		{"lead", 9, 9, 9},
		{"latest", 20, 9, 20},
		{"latest", 20, 9, 20},
		{"latest", 7, 9, 20},
		{"latest", domain.LeadershipGeneration(^uint64(0)), 9, 20},
	}
	for _, s := range steps {
		var err error
		if s.call == "lead" {
			err = f.store.UpdateLeadershipGeneration(ctx, s.gen)
		} else {
			err = f.store.UpdateLatestKnownGeneration(ctx, s.gen)
		}
		if err != nil {
			t.Fatal(err)
		}
		var lead, latest int64
		if err := f.db.QueryRow(`select leadership_generation, latest_known_generation from installation_identity`).Scan(&lead, &latest); err != nil {
			t.Fatal(err)
		}
		if lead != s.lead || latest != s.latest {
			t.Fatalf("%s %d: stored %d/%d want %d/%d", s.call, s.gen, lead, latest, s.lead, s.latest)
		}
	}
	got, err := f.store.Get(ctx)
	if err != nil || got.LatestKnownGeneration != 20 || got.LeadershipGeneration != 9 {
		t.Fatalf("get: %+v %v", got, err)
	}
}

func TestSQLiteStoreClusterKeyRoundTrip(t *testing.T) {
	f := newSQLiteIdentityFixture(t)
	ctx := context.Background()
	if _, _, err := f.store.Ensure(ctx, sqliteInstA, sqliteNodeA); err != nil {
		t.Fatal(err)
	}
	key := uuid.MustParse("CCCCCCCC-1111-4111-8111-111111111111")
	if err := f.store.UpdateClusterKeyID(ctx, &key); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := f.db.QueryRow(`select cluster_key_id from installation_identity`).Scan(&stored); err != nil || stored != "cccccccc-1111-4111-8111-111111111111" {
		t.Fatalf("stored %q %v", stored, err)
	}
	got, _ := f.store.Get(ctx)
	if got.ClusterKeyID == nil || *got.ClusterKeyID != key || got.ClusterKeyID.String() != stored {
		t.Fatalf("read %v", got.ClusterKeyID)
	}
	if err := f.store.UpdateClusterKeyID(ctx, nil); err != nil {
		t.Fatal(err)
	}
	var isNull bool
	if err := f.db.QueryRow(`select cluster_key_id is null from installation_identity`).Scan(&isNull); err != nil || !isNull {
		t.Fatalf("expected SQL NULL: %v %v", isNull, err)
	}
	zero := uuid.Nil
	if err := f.store.UpdateClusterKeyID(ctx, &zero); err != nil {
		t.Fatal(err)
	}
	if got, _ = f.store.Get(ctx); got.ClusterKeyID == nil || *got.ClusterKeyID != uuid.Nil {
		t.Fatalf("nil uuid round trip: %v", got.ClusterKeyID)
	}
}

func TestSQLiteStoreMissingRowErrorsAreNotFound(t *testing.T) {
	f := newSQLiteIdentityFixture(t)
	ctx := context.Background()
	calls := map[string]func() error{
		"role":   func() error { return f.store.UpdateRole(ctx, domain.TopologyRoleNode) },
		"lead":   func() error { return f.store.UpdateLeadershipGeneration(ctx, 2) },
		"latest": func() error { return f.store.UpdateLatestKnownGeneration(ctx, 2) },
		"key":    func() error { k := uuid.New(); return f.store.UpdateClusterKeyID(ctx, &k) },
	}
	for name, call := range calls {
		if err := call(); err != ErrNotFound {
			t.Errorf("%s: got %v want ErrNotFound", name, err)
		}
	}
	if _, err := f.store.Get(ctx); err != ErrNotFound {
		t.Fatalf("Get: %v", err)
	}
	if f.rowCount(t) != 0 {
		t.Fatal("update created a row")
	}
}

func TestSQLiteStoreTimestampsUseTaggedClock(t *testing.T) {
	f := newSQLiteIdentityFixture(t)
	ctx := context.Background()
	id, _, err := f.store.Ensure(ctx, sqliteInstA, sqliteNodeA)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: 1_700_000_001, Nanos: 123})
	if raw := f.rawUpdatedAt(t); raw != *want {
		t.Fatalf("raw updated_at %q want %q", raw, *want)
	}
	if id.UpdatedAt.Unix() != 1_700_000_001 || id.UpdatedAt.Nanosecond() != 123 || id.UpdatedAt.Location() != nil && id.UpdatedAt.Location().String() != "UTC" {
		t.Fatalf("Ensure UpdatedAt %v", id.UpdatedAt)
	}
	previous := f.rawUpdatedAt(t)
	updates := []func() error{
		func() error { return f.store.UpdateRole(ctx, domain.TopologyRolePrimary) },
		func() error { return f.store.UpdateLeadershipGeneration(ctx, 1) },
		func() error { return f.store.UpdateLatestKnownGeneration(ctx, 1) }, // no-op value, still stamps like PG
		func() error { return f.store.UpdateClusterKeyID(ctx, nil) },
	}
	for i, update := range updates {
		if err := update(); err != nil {
			t.Fatal(err)
		}
		raw := f.rawUpdatedAt(t)
		if raw <= previous || len(raw) != 31 || raw[0] != '1' {
			t.Fatalf("update %d: %q not after %q", i, raw, previous)
		}
		previous = raw
		got, err := f.store.Get(ctx)
		if err != nil || got.UpdatedAt.Unix() != 1_700_000_002+int64(i) {
			t.Fatalf("update %d decoded %v %v", i, got.UpdatedAt, err)
		}
	}
}

func TestSQLiteStoreFailedWriteLeavesRowUnchanged(t *testing.T) {
	f := newSQLiteIdentityFixture(t)
	ctx := context.Background()
	// Ensure: a failing insert must not leave a row behind.
	if _, err := f.db.Exec(`create trigger boom_insert before insert on installation_identity begin select raise(abort,'boom'); end`); err != nil {
		t.Fatal(err)
	}
	if _, created, err := f.store.Ensure(ctx, sqliteInstA, sqliteNodeA); err == nil || created || f.rowCount(t) != 0 {
		t.Fatalf("Ensure under failing insert: err=%v created=%v rows=%d", err, created, f.rowCount(t))
	}
	if _, err := f.db.Exec(`drop trigger boom_insert`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.store.Ensure(ctx, sqliteInstA, sqliteNodeA); err != nil {
		t.Fatal(err)
	}
	before, _ := f.store.Get(ctx)
	rawBefore := f.rawUpdatedAt(t)
	// A trigger that aborts after the first column write must roll the whole
	// UPDATE back, including the max() side effect and the stamp.
	if _, err := f.db.Exec(`create trigger boom_update after update on installation_identity begin select raise(abort,'boom'); end`); err != nil {
		t.Fatal(err)
	}
	failures := map[string]error{
		"role":   f.store.UpdateRole(ctx, domain.TopologyRoleNode),
		"lead":   f.store.UpdateLeadershipGeneration(ctx, 50),
		"latest": f.store.UpdateLatestKnownGeneration(ctx, 50),
		"key":    func() error { k := uuid.New(); return f.store.UpdateClusterKeyID(ctx, &k) }(),
	}
	for name, err := range failures {
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Errorf("%s: %v", name, err)
		}
	}
	after, err := f.store.Get(ctx)
	if err != nil || after != before || f.rawUpdatedAt(t) != rawBefore {
		t.Fatalf("row changed: %+v vs %+v (%v)", after, before, err)
	}
}

func TestSQLiteStoreContextAndClosedDatabase(t *testing.T) {
	f := newSQLiteIdentityFixture(t)
	if _, _, err := f.store.Ensure(context.Background(), sqliteInstA, sqliteNodeA); err != nil {
		t.Fatal(err)
	}
	before, _ := f.store.Get(context.Background())
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	k := uuid.New()
	check := func(label string, ctx context.Context) {
		t.Helper()
		_, getErr := f.store.Get(ctx)
		_, _, ensureErr := f.store.Ensure(ctx, sqliteInstA, sqliteNodeA)
		for name, err := range map[string]error{
			"get": getErr, "ensure": ensureErr,
			"role":   f.store.UpdateRole(ctx, domain.TopologyRoleNode),
			"lead":   f.store.UpdateLeadershipGeneration(ctx, 7),
			"latest": f.store.UpdateLatestKnownGeneration(ctx, 7),
			"key":    f.store.UpdateClusterKeyID(ctx, &k),
		} {
			if err == nil || errors.Is(err, ErrNotFound) {
				t.Errorf("%s %s: %v", label, name, err)
			}
		}
	}
	check("canceled", canceled)
	if err := f.store.UpdateRole(canceled, "bogus"); err == nil || err.Error() != "invalid role: bogus" {
		t.Errorf("role validation precedes context: %v", err)
	}
	if after, _ := f.store.Get(context.Background()); after != before {
		t.Fatalf("canceled calls changed the row: %+v", after)
	}
	f.db.Close()
	check("closed", context.Background())
}

func TestSQLiteStoreNilReceiversAndInputs(t *testing.T) {
	f := newSQLiteIdentityFixture(t)
	var typedNil *SQLiteStore
	var asStore Store = typedNil
	stores := map[string]Store{
		"typed nil":  asStore,
		"nil db":     NewSQLiteStore(nil, sqlitedb.NewWriter(f.db, nil)),
		"nil writer": NewSQLiteStore(f.db, nil),
	}
	k := uuid.New()
	for name, s := range stores {
		_, getErr := s.Get(context.Background())
		_, _, ensureErr := s.Ensure(context.Background(), sqliteInstA, sqliteNodeA)
		for call, err := range map[string]error{
			"get": getErr, "ensure": ensureErr,
			"role":   s.UpdateRole(context.Background(), domain.TopologyRoleNode),
			"lead":   s.UpdateLeadershipGeneration(context.Background(), 2),
			"latest": s.UpdateLatestKnownGeneration(context.Background(), 2),
			"key":    s.UpdateClusterKeyID(context.Background(), &k),
		} {
			if name == "nil writer" && (call == "get") {
				continue // reads need no writer
			}
			if err == nil || errors.Is(err, ErrNotFound) {
				t.Errorf("%s %s: %v", name, call, err)
			}
		}
	}
	//nolint:staticcheck // deliberately nil
	if _, err := f.store.Get(nil); err == nil {
		t.Error("nil ctx Get must error, not panic")
	}
	//nolint:staticcheck
	if err := f.store.UpdateRole(nil, domain.TopologyRoleNode); err == nil {
		t.Error("nil ctx UpdateRole must error, not panic")
	}
}

func TestSQLiteStoreGetRejectsCorruptRows(t *testing.T) {
	ctx := context.Background()
	corrupt := func(t *testing.T, f *sqliteIdentityFixture, set string) {
		t.Helper()
		conn, err := f.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.ExecContext(ctx, `pragma ignore_check_constraints=1`); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.ExecContext(ctx, `update installation_identity set `+set); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.ExecContext(ctx, `pragma ignore_check_constraints=0`); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct{ name, set, want string }{
		{"bad role", `role='wizard'`, "invalid role in installation_identity: wizard"},
		{"negative infinity stamp", `updated_at='0'`, "updated_at"},
		{"noncanonical stamp", `updated_at='2026-01-01'`, "updated_at"},
		{"bad cluster key", `cluster_key_id='not-a-uuid'`, "cluster_key_id"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newSQLiteIdentityFixture(t)
			if _, _, err := f.store.Ensure(ctx, sqliteInstA, sqliteNodeA); err != nil {
				t.Fatal(err)
			}
			corrupt(t, f, c.set)
			if _, err := f.store.Get(ctx); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Get: %v", err)
			}
		})
	}
}

func TestSQLiteStoreEnsureHitDoesNotWrite(t *testing.T) {
	f := newSQLiteIdentityFixture(t)
	ctx := context.Background()
	if _, _, err := f.store.Ensure(ctx, sqliteInstA, sqliteNodeA); err != nil {
		t.Fatal(err)
	}
	raw := f.rawUpdatedAt(t)
	tick := f.tick
	if _, created, err := f.store.Ensure(ctx, sqliteInstA, sqliteNodeA); err != nil || created {
		t.Fatalf("%v %v", err, created)
	}
	if f.rawUpdatedAt(t) != raw || f.tick != tick {
		t.Fatalf("hit must not stamp or take the write lock (tick %d -> %d)", tick, f.tick)
	}
}

func TestSQLiteStoreUnmigratedDatabaseSurfacesError(t *testing.T) {
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "raw.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := NewSQLiteStore(db, sqlitedb.NewWriter(db, nil))
	if _, err := s.Get(context.Background()); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Get: %v", err)
	}
	if _, _, err := s.Ensure(context.Background(), sqliteInstA, sqliteNodeA); err == nil {
		t.Fatal("Ensure on unmigrated db must fail")
	}
}

// A caller that missed the fast-path read while another writer held the lock
// must return that writer's row with created=false, not hit the primary key.
func TestSQLiteStoreEnsureRechecksInsideWrite(t *testing.T) {
	f := newSQLiteIdentityFixture(t)
	ctx := context.Background()
	type result struct {
		id      Identity
		created bool
		err     error
	}
	results := make(chan result, 4)
	held := make(chan struct{})
	release := make(chan struct{})
	holder := make(chan error, 1)
	go func() {
		holder <- f.store.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
			if _, err := v.Conn().ExecContext(v.Context(),
				`insert into installation_identity (id,installation_id,node_id,role,leadership_generation,latest_known_generation,updated_at)
				 values ('default',?,?,'primary',4,4,?)`, sqliteInstA, sqliteNodeA, v.Timestamp()); err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	for i := 0; i < cap(results); i++ {
		go func() {
			id, created, err := f.store.Ensure(ctx, domain.InstallationID(uuid.NewString()), domain.NodeID(uuid.NewString()))
			results <- result{id, created, err}
		}()
	}
	// The committed row is invisible to the fast-path read until release, so
	// every Ensure misses and queues for the write lock.
	time.Sleep(300 * time.Millisecond)
	close(release)
	if err := <-holder; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < cap(results); i++ {
		r := <-results
		if r.err != nil || r.created || r.id.InstallationID != sqliteInstA || r.id.Role != domain.TopologyRolePrimary || r.id.LeadershipGeneration != 4 {
			t.Fatalf("queued Ensure: %+v created=%v err=%v", r.id, r.created, r.err)
		}
	}
}
