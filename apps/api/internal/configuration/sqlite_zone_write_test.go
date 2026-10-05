package configuration

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

func createZoneContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func createZoneZero(t *testing.T, got AutoApplyResult[domain.ZoneState], err error) {
	t.Helper()
	if err == nil || !reflect.DeepEqual(got, AutoApplyResult[domain.ZoneState]{}) {
		t.Fatalf("expected error and zero result: %+v, %v", got, err)
	}
}

func createZoneRollback(t *testing.T, db *sql.DB, before string) {
	t.Helper()
	if snapshotSettingsBytes(t, db) != before {
		t.Fatal("settings changed across rollback")
	}
	for _, table := range []string{"zones", "config_revisions", "apply_jobs"} {
		if enqueueCount(t, db, table) != 0 {
			t.Fatal("rollback left rows in", table)
		}
	}
}

func TestSQLiteCreateZoneNormalizationPriority(t *testing.T) {
	for _, name := range []string{"", " ... ", "*.example.org", "a..org", "-a.org", "a_.org", strings.Repeat("a", 64), strings.Repeat("a.", 127) + "a", strings.Repeat(strings.Repeat("a", 63)+".", 3) + strings.Repeat("b", 62)} {
		t.Run(name, func(t *testing.T) {
			_, want := domain.NormalizeDNSName(name, false)
			if want == nil {
				t.Fatal("invalid test input")
			}
			db, _ := enqueueFixture(t)
			calls := 0
			writer := sqlitedb.NewWriter(db, func() sqlitedb.FiniteTimestamp {
				calls++
				return sqlitedb.FiniteTimestamp{}
			})
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			for _, entry := range []struct {
				writer *sqlitedb.Writer
				ctx    context.Context
			}{{writer, createZoneContext(t)}, {writer, ctx}, {nil, ctx}} {
				got, err := NewSQLiteZoneWriteStore(entry.writer).CreateZone(entry.ctx, name, "bad actor")
				createZoneZero(t, got, err)
				if err.Error() != want.Error() {
					t.Fatal("normalization lost precedence", err, want)
				}
			}
			if calls != 0 || enqueueCount(t, db, "zones") != 0 {
				t.Fatal("invalid name touched transaction")
			}
		})
	}
}

func TestSQLiteCreateZoneCommittedState(t *testing.T) {
	db, path := enqueueFixture(t)
	db.SetMaxOpenConns(1)
	settingsExec(t, db, "update installation_settings set current_applied_revision_id='retained',forwarding_rules='{}'")
	calls := 0
	writer := sqlitedb.NewWriter(db, func() sqlitedb.FiniteTimestamp {
		calls++
		return sqlitedb.FiniteTimestamp{Seconds: 123, Nanos: 456}
	})
	got, err := NewSQLiteZoneWriteStore(writer).CreateZone(createZoneContext(t), "  MiXeD.Example...  ", "")
	if err != nil {
		t.Fatal(err)
	}
	zone := got.Value
	canonical, err := sqlitedb.ParseUUIDLookupText(zone.ID)
	if err != nil || canonical != zone.ID || zone.ID[14] != '4' || !strings.ContainsRune("89ab", rune(zone.ID[19])) || zone.Name != "mixed.example" || !zone.Enabled || zone.Records == nil || len(zone.Records) != 0 || calls != 1 {
		t.Fatal("zone or captured clock", zone, calls, err)
	}
	wantJob := JobRecord{ID: got.Apply.Job.ID, RevisionID: got.Apply.RevisionID, Target: "combined", Status: "queued", Source: "ordinary", CorrelationID: got.Apply.Job.CorrelationID, CreatedAt: time.Unix(123, 456)}
	if !reflect.DeepEqual(got.Apply.Job, wantJob) {
		t.Fatalf("job defaults: %+v", got.Apply.Job)
	}
	ids := map[string]bool{}
	for _, id := range []string{zone.ID, got.Apply.RevisionID, got.Apply.Job.ID, got.Apply.Job.CorrelationID} {
		canonical, err := sqlitedb.ParseUUIDLookupText(id)
		if err != nil || canonical != id || ids[id] {
			t.Fatal("noncanonical or reused generated ID", id, err)
		}
		ids[id] = true
	}
	reopened, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	stored, err := NewSQLiteZoneReadStore(reopened).GetZone(createZoneContext(t), zone.ID)
	if err != nil || !reflect.DeepEqual(stored, zone) {
		t.Fatal("reopened zone", stored, err)
	}
	var name, created, updated, raw, checksum, revisionClock, jobClock, settingsClock, applied, desired string
	var enabled, number int
	var revisionActor, jobActor *string
	if err := reopened.QueryRow("select name,enabled,created_at,updated_at from zones where id=?", zone.ID).Scan(&name, &enabled, &created, &updated); err != nil {
		t.Fatal(err)
	}
	if err := reopened.QueryRow("select snapshot,checksum,created_at,revision_number,actor_user_id from config_revisions where id=?", got.Apply.RevisionID).Scan(&raw, &checksum, &revisionClock, &number, &revisionActor); err != nil {
		t.Fatal(err)
	}
	var jobRevision, target, status, source, correlation string
	if err := reopened.QueryRow("select created_at,actor_user_id,revision_id,target,status,source,correlation_id from apply_jobs where id=?", got.Apply.Job.ID).Scan(&jobClock, &jobActor, &jobRevision, &target, &status, &source, &correlation); err != nil {
		t.Fatal(err)
	}
	if jobRevision != got.Apply.RevisionID || target != "combined" || status != "queued" || source != "ordinary" || correlation != got.Apply.Job.CorrelationID {
		t.Fatal("persisted job differs from returned job")
	}
	if err := reopened.QueryRow("select updated_at,current_applied_revision_id,current_desired_revision_id from installation_settings").Scan(&settingsClock, &applied, &desired); err != nil {
		t.Fatal(err)
	}
	snapshot, err := NewSQLiteSnapshotStore(reopened, settingsWriter(reopened)).Snapshot(createZoneContext(t))
	stamp, _ := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: 123, Nanos: 456})
	if err != nil || len(snapshot.Zones) != 1 || !reflect.DeepEqual(snapshot.Zones[0], zone) || raw != domain.StableStringify(snapshot) || checksum != domain.ChecksumSnapshot(snapshot) || name != zone.Name || enabled != 1 || number != 1 || revisionActor != nil || jobActor != nil || applied != "retained" || desired != got.Apply.RevisionID || created != *stamp || updated != created || revisionClock != created || jobClock != created || settingsClock != created || calls != 1 {
		t.Fatal("persisted state differs", snapshot, err)
	}
	if snapshot.Settings.DefaultPool == nil || snapshot.Settings.Ingress.IPv4 != "" || len(snapshot.Settings.ForwardingRules) != 0 || len(snapshot.Streams) != 0 || len(snapshot.Certificates) != 0 {
		t.Fatal("snapshot defaults", snapshot)
	}
}

func TestSQLiteCreateZoneValidBoundariesAndFreshIDs(t *testing.T) {
	db, _ := enqueueFixture(t)
	store := NewSQLiteZoneWriteStore(settingsWriter(db))
	ids := map[string]bool{}
	for _, name := range []string{strings.Repeat("a", 63), strings.Repeat(strings.Repeat("b", 63)+".", 3) + strings.Repeat("c", 61)} {
		got, err := store.CreateZone(createZoneContext(t), name, "")
		if err != nil || got.Value.Name != name || ids[got.Value.ID] {
			t.Fatal("valid boundary or fresh zone ID", got, err)
		}
		ids[got.Value.ID] = true
	}
	for _, table := range []string{"zones", "config_revisions", "apply_jobs"} {
		if enqueueCount(t, db, table) != 2 {
			t.Fatal("different zones must change snapshot", table)
		}
	}
}

func TestSQLiteCreateZoneActorBoundary(t *testing.T) {
	actorID := "abcdef01-2345-1234-0000-abcdef012345"
	for _, actor := range []string{"", actorID, "{ABCDEF01234512340000ABCDEF012345}", "bad", " " + actorID, snapshotCertID} {
		t.Run(actor, func(t *testing.T) {
			db, _ := enqueueFixture(t)
			settingsExec(t, db, `insert into users (id,username,password_hash,role,created_at,updated_at) values (?,'fixture','hash','owner','0','0')`, actorID)
			before := snapshotSettingsBytes(t, db)
			store := NewSQLiteZoneWriteStore(settingsWriter(db))
			got, err := store.CreateZone(createZoneContext(t), "actor.example", actor)
			if actor == "" || actor == actorID || strings.HasPrefix(actor, "{") {
				var revisionActor, jobActor *string
				if err != nil {
					t.Fatal(err)
				}
				if err := db.QueryRow("select actor_user_id from config_revisions").Scan(&revisionActor); err != nil {
					t.Fatal(err)
				}
				if err := db.QueryRow("select actor_user_id from apply_jobs").Scan(&jobActor); err != nil {
					t.Fatal(err)
				}
				if actor == "" {
					if revisionActor != nil || jobActor != nil || got.Apply.Job.ActorUserID != nil {
						t.Fatal("empty actor must be NULL")
					}
				} else if revisionActor == nil || jobActor == nil || *revisionActor != actorID || *jobActor != actorID || got.Apply.Job.ActorUserID == nil || *got.Apply.Job.ActorUserID != actorID {
					t.Fatal("actor alias was not canonicalized")
				}
			} else {
				createZoneZero(t, got, err)
				createZoneRollback(t, db, before)
				if _, err := store.CreateZone(createZoneContext(t), "recovery.example", ""); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestSQLiteCreateZonePoolAndSnapshotPriority(t *testing.T) {
	for _, pool := range []any{nil, "null", "{}", "[]", `{"id":" ","endpoints":[{"host":"192.0.2.1","port":53}]}`, `{"id":"main","endpoints":[{"host":"name","port":53}]}`, `{"id":"main","endpoints":[{"host":"::1","port":65536}]}`} {
		t.Run(domain.StableStringify(pool), func(t *testing.T) {
			for _, failure := range []string{"pool", "stream", "certificate"} {
				t.Run(failure, func(t *testing.T) {
					db, _ := enqueueFixture(t)
					settingsExec(t, db, "update installation_settings set default_resolver_pool=?", pool)
					if failure == "stream" {
						settingsExec(t, db, `insert into stream_routes (id,protocol,listen_address,listen_port,upstream,created_at,updated_at) values (?,'tcp','0.0.0.0',0,'[]','0','0')`, snapshotCertID)
					}
					if failure == "certificate" {
						if err := snapshotCertificateInsert(createZoneContext(t), db, snapshotCertID, `[]`, "2", nil); err != nil {
							t.Fatal(err)
						}
					}
					before := snapshotSettingsBytes(t, db)
					got, err := NewSQLiteZoneWriteStore(settingsWriter(db)).CreateZone(createZoneContext(t), "priority.example", "bad actor")
					createZoneZero(t, got, err)
					poolError := "Configure a default resolver pool before apply"
					if (err.Error() == poolError) != (failure == "pool") || strings.Contains(err.Error(), "UUID") {
						t.Fatal("snapshot/pool/actor priority", failure, err)
					}
					createZoneRollback(t, db, before)
				})
			}
		})
	}
	t.Run("initialization rolls back", func(t *testing.T) {
		db, _, _ := sqliteSettingsFixture(t)
		got, err := NewSQLiteZoneWriteStore(settingsWriter(db)).CreateZone(createZoneContext(t), "empty.example", "")
		createZoneZero(t, got, err)
		if enqueueCount(t, db, "installation_settings") != 0 || enqueueCount(t, db, "zones") != 0 {
			t.Fatal("initialization escaped rollback")
		}
	})
}

func TestSQLiteCreateZoneDuplicateBeforeEnqueue(t *testing.T) {
	db, _ := enqueueFixture(t)
	store := NewSQLiteZoneWriteStore(settingsWriter(db))
	first, err := store.CreateZone(createZoneContext(t), "same.example", "")
	if err != nil {
		t.Fatal(err)
	}
	settingsExec(t, db, "update installation_settings set default_resolver_pool=NULL")
	before, revision := snapshotSettingsBytes(t, db), enqueueRevisionBytes(t, db)
	got, err := store.CreateZone(createZoneContext(t), " SAME.Example... ", "bad actor")
	createZoneZero(t, got, err)
	if !strings.Contains(err.Error(), "UNIQUE") || before != snapshotSettingsBytes(t, db) || revision != enqueueRevisionBytes(t, db) {
		t.Fatal("duplicate did not precede enqueue", err)
	}
	stored, err := NewSQLiteZoneReadStore(db).GetZone(createZoneContext(t), first.Value.ID)
	if err != nil || !reflect.DeepEqual(stored, first.Value) || enqueueCount(t, db, "zones") != 1 || enqueueCount(t, db, "config_revisions") != 1 || enqueueCount(t, db, "apply_jobs") != 1 {
		t.Fatal("duplicate changed existing state", stored, err)
	}
}

func TestSQLiteCreateZoneStagedRollback(t *testing.T) {
	for _, stage := range []string{"zone", "revision", "pointer", "job", "scan", "cancel", "commit"} {
		t.Run(stage, func(t *testing.T) {
			db, _ := enqueueFixture(t)
			db.SetMaxOpenConns(1)
			before := snapshotSettingsBytes(t, db)
			ctx, cancel := context.WithCancel(createZoneContext(t))
			defer cancel()
			writer := settingsWriter(db)
			trigger := map[string]string{"zone": "after insert on zones", "revision": "after insert on config_revisions", "pointer": "after update of current_desired_revision_id on installation_settings", "job": "after insert on apply_jobs"}[stage]
			if trigger != "" {
				settingsExec(t, db, "create temp trigger create_zone_failure "+trigger+" begin select raise(ABORT,'injected failure'); end")
			}
			if stage == "scan" || stage == "cancel" {
				writer = sqlitedb.NewWriter(db, func() sqlitedb.FiniteTimestamp {
					if stage == "cancel" {
						cancel()
						return sqlitedb.FiniteTimestamp{}
					}
					return sqlitedb.FiniteTimestamp{Seconds: math.MaxInt64, Nanos: 99}
				})
			}
			if stage == "commit" {
				// The fixture-only guard references main.users; the trigger is connection-local.
				settingsExec(t, db, `create table create_zone_guard (actor text references users(id) DEFERRABLE INITIALLY DEFERRED)`)
				callbackSucceeded := false
				err := writer.Write(createZoneContext(t), func(v sqlitedb.WriteValue) error {
					_, err := v.Conn().ExecContext(v.Context(), "insert into create_zone_guard values (?)", snapshotCertID)
					callbackSucceeded = err == nil
					return err
				})
				if !callbackSucceeded || err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") || enqueueCount(t, db, "create_zone_guard") != 0 {
					t.Fatal("guard must fail real COMMIT after callback success", err)
				}
				settingsExec(t, db, `create temp trigger create_zone_failure after insert on apply_jobs begin insert into create_zone_guard values ('00000000-0000-0000-0000-000000000001'); end`)
			}
			got, err := NewSQLiteZoneWriteStore(writer).CreateZone(ctx, "rollback.example", "")
			createZoneZero(t, got, err)
			if stage == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if stage == "commit" && !strings.Contains(err.Error(), "FOREIGN KEY") {
				t.Fatal("expected actual COMMIT failure", err)
			}
			createZoneRollback(t, db, before)
			if trigger != "" || stage == "commit" {
				settingsExec(t, db, "drop trigger create_zone_failure")
			}
			if stage == "commit" {
				settingsExec(t, db, "drop table create_zone_guard")
			}
			if _, err := NewSQLiteZoneWriteStore(settingsWriter(db)).CreateZone(createZoneContext(t), "recovery.example", ""); err != nil {
				t.Fatal("connection did not recover", err)
			}
		})
	}
}

func TestSQLiteCreateZoneCanceledEntry(t *testing.T) {
	db, _ := enqueueFixture(t)
	before := snapshotSettingsBytes(t, db)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := NewSQLiteZoneWriteStore(settingsWriter(db)).CreateZone(ctx, "valid.example", "")
	createZoneZero(t, got, err)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	createZoneRollback(t, db, before)
}

func TestSQLiteCreateZoneIndependentContention(t *testing.T) {
	db, path := enqueueFixture(t)
	other, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	db.SetMaxOpenConns(1)
	other.SetMaxOpenConns(1)
	ctx := createZoneContext(t)
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]AutoApplyResult[domain.ZoneState], 2)
	errs := make([]error, 2)
	for i, conn := range []*sql.DB{db, other} {
		wg.Add(1)
		go func(i int, conn *sql.DB) {
			defer wg.Done()
			<-start
			results[i], errs[i] = NewSQLiteZoneWriteStore(settingsWriter(conn)).CreateZone(ctx, []string{" SAME.Example... ", "same.example"}[i], "")
		}(i, conn)
	}
	close(start)
	wg.Wait()
	successes := 0
	for i, err := range errs {
		if err == nil {
			successes++
		} else {
			createZoneZero(t, results[i], err)
			if !strings.Contains(err.Error(), "UNIQUE") {
				t.Fatal("contention loser did not reach name constraint", err)
			}
		}
	}
	if successes != 1 {
		t.Fatal("expected exactly one committed winner", errs)
	}
	for _, table := range []string{"zones", "config_revisions", "apply_jobs"} {
		if enqueueCount(t, other, table) != 1 {
			t.Fatal("duplicated source effects", table)
		}
	}
}

func TestSQLiteCreateZoneClosedDatabase(t *testing.T) {
	db, _ := enqueueFixture(t)
	store := NewSQLiteZoneWriteStore(settingsWriter(db))
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := createZoneContext(t)
	got, err := store.CreateZone(ctx, "valid.example", "")
	createZoneZero(t, got, err)

	const invalidName = "*.example"
	_, want := domain.NormalizeDNSName(invalidName, false)
	if want == nil {
		t.Fatal("invalid test input")
	}
	got, err = store.CreateZone(ctx, invalidName, "")
	createZoneZero(t, got, err)
	if err.Error() != want.Error() {
		t.Fatal("normalization lost precedence over closed database", err, want)
	}
}
