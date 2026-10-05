package configuration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

const enqueuePool = `{"id":"main","endpoints":[{"host":"192.0.2.53","port":53}]}`

func enqueueFixture(t *testing.T) (*sql.DB, string) {
	t.Helper()
	db, _, path := sqliteSettingsFixture(t)
	settingsExec(t, db, `insert into installation_settings
		(id,default_resolver_pool,forwarding_rules,created_at,updated_at)
		values ('default',?,'[]','0','0')`, enqueuePool)
	return db, path
}

func enqueueCall(t *testing.T, db *sql.DB, actor string) ApplyResult {
	t.Helper()
	var result ApplyResult
	err := settingsWriter(db).Write(context.Background(), func(v sqlitedb.WriteValue) error {
		var err error
		result, err = sqliteCreateApplyJobInTransaction(v, actor)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func enqueueCount(t *testing.T, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, table string) int {
	t.Helper()
	var count int
	if err := q.QueryRowContext(context.Background(), "select count(*) from "+table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func enqueueRevisionBytes(t *testing.T, db *sql.DB) string {
	t.Helper()
	var raw string
	err := db.QueryRow(`select json_array(id,revision_number,checksum,snapshot,actor_user_id,
		created_at,applied_at,source,source_primary_id,source_node_id,source_revision_id,
		snapshot_content_hash,snapshot_version,replication_version,leadership_generation) from config_revisions`).Scan(&raw)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestSQLiteEnqueueNewAndReuse(t *testing.T) {
	db, _ := enqueueFixture(t)
	first := enqueueCall(t, db, "")
	second := enqueueCall(t, db, "")
	if first.RevisionID != second.RevisionID || first.Job.ID == second.Job.ID || first.Job.CorrelationID == second.Job.CorrelationID {
		t.Fatal(first, second)
	}
	var number, revisions, jobs int
	if err := db.QueryRow("select revision_number from config_revisions").Scan(&number); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("select count(*) from config_revisions").Scan(&revisions); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("select count(*) from apply_jobs").Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if number != 1 || revisions != 1 || jobs != 2 {
		t.Fatal(number, revisions, jobs)
	}
}

func TestSQLiteEnqueueDefaultsClockAndChangedSnapshot(t *testing.T) {
	db, _ := enqueueFixture(t)
	settingsExec(t, db, "update installation_settings set current_applied_revision_id='retained',forwarding_rules='{}'")
	got := enqueueCall(t, db, "")
	want := JobRecord{ID: got.Job.ID, RevisionID: got.RevisionID, Target: "combined", Status: "queued",
		Source: "ordinary", CorrelationID: got.Job.CorrelationID, CreatedAt: time.Unix(123, 456)}
	if !reflect.DeepEqual(got.Job, want) || got.Job.CreatedAt.Location() != time.Local {
		t.Fatalf("job: %+v", got.Job)
	}
	var created, updated, revisionClock, jobClock, applied, desired, source, raw string
	var actor, appliedClock, primary, node, sourceRevision, hash *string
	var version, replication, leadership *int64
	err := db.QueryRow(`select created_at,updated_at,current_applied_revision_id,current_desired_revision_id from installation_settings`).Scan(&created, &updated, &applied, &desired)
	if err != nil {
		t.Fatal(err)
	}
	err = db.QueryRow(`select created_at,snapshot,actor_user_id,applied_at,source,source_primary_id,
		source_node_id,source_revision_id,snapshot_content_hash,snapshot_version,replication_version,
		leadership_generation from config_revisions`).Scan(&revisionClock, &raw, &actor, &appliedClock, &source, &primary, &node, &sourceRevision, &hash, &version, &replication, &leadership)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("select created_at from apply_jobs").Scan(&jobClock); err != nil {
		t.Fatal(err)
	}
	stamp, _ := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: 123, Nanos: 456})
	if created != "0" || updated != *stamp || revisionClock != updated || jobClock != updated || applied != "retained" || desired != got.RevisionID || source != "ordinary" || actor != nil || appliedClock != nil || primary != nil || node != nil || sourceRevision != nil || hash != nil || version != nil || replication != nil || leadership != nil {
		t.Fatal("defaults or timestamps changed", created, updated, revisionClock, jobClock, applied, desired)
	}
	snapshot, err := NewSQLiteSnapshotStore(db, settingsWriter(db)).Snapshot(context.Background())
	if err != nil || raw != domain.StableStringify(snapshot) || snapshot.Settings.Ingress.IPv4 != "" || len(snapshot.Settings.ForwardingRules) != 0 {
		t.Fatal(raw, snapshot, err)
	}
	settingsExec(t, db, "update installation_settings set ingress_ipv4='192.0.2.10'")
	changed := enqueueCall(t, db, "")
	var number int
	if err := db.QueryRow("select revision_number from config_revisions where id=?", changed.RevisionID).Scan(&number); err != nil || number != 2 || changed.RevisionID == got.RevisionID {
		t.Fatal(changed, number, err)
	}
}

func TestSQLiteEnqueueReusePreservesRevision(t *testing.T) {
	db, _ := enqueueFixture(t)
	first := enqueueCall(t, db, "")
	settingsExec(t, db, `update config_revisions set created_at='0',applied_at='2',source='sync',
		source_primary_id=?,source_node_id=?,source_revision_id=?,snapshot_content_hash='old',
		snapshot_version=3,replication_version=4,leadership_generation=5`, snapshotCertID, snapshotCertID, snapshotCertID)
	before := enqueueRevisionBytes(t, db)
	second := enqueueCall(t, db, "")
	if first.RevisionID != second.RevisionID || before != enqueueRevisionBytes(t, db) || first.Job.ID == second.Job.ID || first.Job.CorrelationID == second.Job.CorrelationID {
		t.Fatal("reuse changed immutable revision or reused job", first, second)
	}
}

func TestSQLiteEnqueueCallerVisibilityAndAbort(t *testing.T) {
	for _, abort := range []bool{false, true} {
		t.Run(fmt.Sprint(abort), func(t *testing.T) {
			db, _ := enqueueFixture(t)
			db.SetMaxOpenConns(1)
			before := snapshotSettingsBytes(t, db)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			stop := errors.New("caller abort after successful enqueue")
			err := settingsWriter(db).Write(ctx, func(v sqlitedb.WriteValue) error {
				if _, err := v.Conn().ExecContext(v.Context(), `insert into zones
					(id,name,created_at,updated_at) values (?,'Pending.Zone.','0','0')`, sqliteZoneID); err != nil {
					return err
				}
				if _, err := v.Conn().ExecContext(v.Context(), `insert into dns_records
					(id,zone_id,name,type,value,created_at,updated_at) values (?,?,'raw','TXT','"pending"','0','0')`, snapshotCertID, sqliteZoneID); err != nil {
					return err
				}
				got, err := sqliteCreateApplyJobInTransaction(v, "")
				if err != nil {
					return err
				}
				var raw string
				if err := v.Conn().QueryRowContext(v.Context(), "select snapshot from config_revisions where id=?", got.RevisionID).Scan(&raw); err != nil {
					return err
				}
				if !strings.Contains(raw, "Pending.Zone.") || !strings.Contains(raw, "pending") {
					t.Fatal("caller mutation missing from snapshot", raw)
				}
				if abort {
					return stop
				}
				return nil
			})
			if abort {
				if !errors.Is(err, stop) || before != snapshotSettingsBytes(t, db) {
					t.Fatal(err, "settings rollback")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			for _, table := range []string{"zones", "dns_records", "config_revisions", "apply_jobs"} {
				want := 1
				if abort {
					want = 0
				}
				if n := enqueueCount(t, db, table); n != want {
					t.Fatal(table, n)
				}
			}
			enqueueCall(t, db, "") // Single-connection cleanup must allow recovery.
		})
	}
}

func TestSQLiteEnqueuePoolAndSnapshotPriority(t *testing.T) {
	for _, pool := range []any{nil, "null", "{}", "[]", `{"id":" ","endpoints":[{"host":"192.0.2.1","port":53}]}`, `{"id":"main","endpoints":[{"host":"name","port":53}]}`, `{"id":"main","endpoints":[{"host":"::1","port":65536}]}`} {
		t.Run(fmt.Sprint(pool), func(t *testing.T) {
			db, _ := enqueueFixture(t)
			settingsExec(t, db, "update installation_settings set default_resolver_pool=?", pool)
			err := settingsWriter(db).Write(context.Background(), func(v sqlitedb.WriteValue) error {
				_, err := sqliteCreateApplyJobInTransaction(v, "bad actor")
				return err
			})
			if err == nil || err.Error() != "Configure a default resolver pool before apply" || enqueueCount(t, db, "config_revisions") != 0 {
				t.Fatal(err)
			}
			settingsExec(t, db, `insert into stream_routes (id,protocol,listen_address,listen_port,upstream,created_at,updated_at)
				values (?,'tcp','0.0.0.0',0,'[]','0','0')`, snapshotCertID)
			err = settingsWriter(db).Write(context.Background(), func(v sqlitedb.WriteValue) error {
				_, err := sqliteCreateApplyJobInTransaction(v, "")
				return err
			})
			if err == nil || err.Error() == "Configure a default resolver pool before apply" {
				t.Fatal("snapshot validation must precede pool error", err)
			}
		})
	}
	t.Run("initialization rollback", func(t *testing.T) {
		db, _, _ := sqliteSettingsFixture(t)
		err := settingsWriter(db).Write(context.Background(), func(v sqlitedb.WriteValue) error {
			_, err := sqliteCreateApplyJobInTransaction(v, "")
			return err
		})
		if err == nil || enqueueCount(t, db, "installation_settings") != 0 {
			t.Fatal(err, "initialization escaped rollback")
		}
	})
}

func TestSQLiteEnqueueActorBoundary(t *testing.T) {
	actorID := "abcdef01-2345-1234-0000-abcdef012345" // Deliberately non-v4/nonstandard variant.
	for _, reuse := range []bool{false, true} {
		for _, actor := range []string{"", "{ABCDEF01234512340000ABCDEF012345}", actorID, "bad", " " + actorID, snapshotCertID} {
			t.Run(fmt.Sprintf("reuse=%v/actor=%s", reuse, actor), func(t *testing.T) {
				db, _ := enqueueFixture(t)
				settingsExec(t, db, `insert into users (id,username,password_hash,role,created_at,updated_at)
					values (?,'fixture','hash','owner','0','0')`, actorID)
				if reuse {
					enqueueCall(t, db, actorID)
					settingsExec(t, db, "update installation_settings set current_desired_revision_id=NULL,updated_at='0'")
				}
				before := snapshotSettingsBytes(t, db)
				var revisionBefore string
				if reuse {
					revisionBefore = enqueueRevisionBytes(t, db)
				}
				valid := actor == "" || actor == actorID || strings.HasPrefix(actor, "{")
				var got ApplyResult
				err := settingsWriter(db).Write(context.Background(), func(v sqlitedb.WriteValue) error {
					var err error
					got, err = sqliteCreateApplyJobInTransaction(v, actor)
					if !valid {
						var desired *string
						if e := v.Conn().QueryRowContext(v.Context(), "select current_desired_revision_id from installation_settings").Scan(&desired); e != nil {
							return e
						}
						if (desired != nil) != reuse || enqueueCount(t, v.Conn(), "config_revisions") != map[bool]int{false: 0, true: 1}[reuse] {
							t.Fatal("actor rejected at wrong SQL boundary", desired)
						}
					}
					return err
				})
				if valid {
					if err != nil || (actor == "" && got.Job.ActorUserID != nil) || (actor != "" && (got.Job.ActorUserID == nil || *got.Job.ActorUserID != actorID)) {
						t.Fatal(got, err)
					}
				} else if err == nil || snapshotSettingsBytes(t, db) != before {
					t.Fatal("invalid actor escaped rollback", err)
				}
				if reuse && revisionBefore != enqueueRevisionBytes(t, db) {
					t.Fatal("reused revision actor/time changed")
				}
			})
		}
	}
}

func TestSQLiteEnqueueStagedFailures(t *testing.T) {
	for _, stage := range []string{"revision", "pointer", "job", "scan"} {
		t.Run(stage, func(t *testing.T) {
			db, _ := enqueueFixture(t)
			db.SetMaxOpenConns(1)
			before := snapshotSettingsBytes(t, db)
			writer := settingsWriter(db)
			if stage == "scan" {
				writer = sqlitedb.NewWriter(db, func() sqlitedb.FiniteTimestamp {
					return sqlitedb.FiniteTimestamp{Seconds: math.MaxInt64, Nanos: 99}
				})
			}
			err := writer.Write(context.Background(), func(v sqlitedb.WriteValue) error {
				trigger := map[string]string{
					"revision": "after insert on config_revisions",
					"pointer":  "after update of current_desired_revision_id on installation_settings",
					"job":      "after insert on apply_jobs",
				}[stage]
				if trigger != "" {
					if _, err := v.Conn().ExecContext(v.Context(), "create temp trigger enqueue_failure "+trigger+" begin select raise(ABORT,'injected enqueue failure'); end"); err != nil {
						return err
					}
				}
				_, err := sqliteCreateApplyJobInTransaction(v, "")
				if stage == "scan" && enqueueCount(t, v.Conn(), "apply_jobs") != 1 {
					t.Fatal("expected scan failure after durable-row insertion")
				}
				return err
			})
			if err == nil || before != snapshotSettingsBytes(t, db) || enqueueCount(t, db, "config_revisions") != 0 || enqueueCount(t, db, "apply_jobs") != 0 {
				t.Fatal("failure did not roll back", err)
			}
			enqueueCall(t, db, "")
		})
	}
}

func TestSQLiteEnqueueNullSerializationAndMaxNumber(t *testing.T) {
	for _, nullSnapshot := range []bool{false, true} {
		t.Run(fmt.Sprint(nullSnapshot), func(t *testing.T) {
			db, _ := enqueueFixture(t)
			if nullSnapshot {
				far, _ := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: time.Date(10000, 6, 1, 0, 0, 0, 0, time.UTC).Unix()})
				if err := snapshotCertificateInsert(context.Background(), db, snapshotCertID, `[]`, far, nil); err != nil {
					t.Fatal(err)
				}
			}
			first := enqueueCall(t, db, "")
			if nullSnapshot {
				var raw, kind, checksum string
				if err := db.QueryRow("select snapshot,typeof(snapshot),checksum from config_revisions").Scan(&raw, &kind, &checksum); err != nil || raw != "null" || kind != "text" || checksum != domain.ChecksumSnapshot(nil) {
					t.Fatal(raw, kind, checksum, err)
				}
			}
			settingsExec(t, db, "update config_revisions set revision_number=2147483647")
			before := enqueueRevisionBytes(t, db)
			second := enqueueCall(t, db, "")
			if second.RevisionID != first.RevisionID || second.Job.ID == first.Job.ID || second.Job.CorrelationID == first.Job.CorrelationID || before != enqueueRevisionBytes(t, db) {
				t.Fatal("reuse at MAX failed", first, second)
			}
			// Removing certificates makes the formerly-null snapshot serializable.
			settingsExec(t, db, "update certificates set expires_at=NULL")
			settingsExec(t, db, "update installation_settings set ingress_ipv4='192.0.2.22'")
			settingsBefore := snapshotSettingsBytes(t, db)
			err := settingsWriter(db).Write(context.Background(), func(v sqlitedb.WriteValue) error {
				_, err := sqliteCreateApplyJobInTransaction(v, "")
				return err
			})
			if err == nil || before != enqueueRevisionBytes(t, db) || settingsBefore != snapshotSettingsBytes(t, db) || enqueueCount(t, db, "apply_jobs") != 2 {
				t.Fatal("MAX+1 escaped int32 CHECK or rollback", err)
			}
		})
	}
}

func TestSQLiteEnqueueConcurrencyAndReopen(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprint(changed), func(t *testing.T) {
			db, path := enqueueFixture(t)
			other, err := sqlitedb.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			db.SetMaxOpenConns(1)
			other.SetMaxOpenConns(1)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			start := make(chan struct{})
			var wg sync.WaitGroup
			results := make([]ApplyResult, 2)
			errs := make([]error, 2)
			for i, conn := range []*sql.DB{db, other} {
				wg.Add(1)
				go func(i int, conn *sql.DB) {
					defer wg.Done()
					<-start
					errs[i] = settingsWriter(conn).Write(ctx, func(v sqlitedb.WriteValue) error {
						if changed {
							if _, err := v.Conn().ExecContext(v.Context(), "update installation_settings set ingress_ipv4=?", fmt.Sprintf("192.0.2.%d", i+1)); err != nil {
								return err
							}
						}
						var err error
						results[i], err = sqliteCreateApplyJobInTransaction(v, "")
						return err
					})
				}(i, conn)
			}
			close(start)
			wg.Wait()
			if errs[0] != nil || errs[1] != nil || results[0].Job.ID == results[1].Job.ID || results[0].Job.CorrelationID == results[1].Job.CorrelationID || (results[0].RevisionID != results[1].RevisionID) != changed {
				t.Fatal(results, errs)
			}
			reopened, err := sqlitedb.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			want := 1
			if changed {
				want = 2
			}
			var max int
			if err := reopened.QueryRow("select max(revision_number) from config_revisions").Scan(&max); err != nil || max != want || enqueueCount(t, reopened, "config_revisions") != want || enqueueCount(t, reopened, "apply_jobs") != 2 {
				t.Fatal(max, err)
			}
			var raw string
			stamp, _ := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: 123, Nanos: 456})
			if err := reopened.QueryRow("select min(created_at) from apply_jobs").Scan(&raw); err != nil || raw != *stamp {
				t.Fatal(raw, err)
			}
		})
	}
}

func TestSQLiteEnqueueMalformedPoolAndInitializationFailure(t *testing.T) {
	t.Run("malformed JSON fallback", func(t *testing.T) {
		db, _ := enqueueFixture(t)
		db.SetMaxOpenConns(1)
		// Deliberately corrupt only disposable fixture data, preserving schema.
		settingsExec(t, db, "PRAGMA ignore_check_constraints=ON")
		settingsExec(t, db, "update installation_settings set default_resolver_pool='{'")
		settingsExec(t, db, "PRAGMA ignore_check_constraints=OFF")
		err := settingsWriter(db).Write(context.Background(), func(v sqlitedb.WriteValue) error {
			_, err := sqliteCreateApplyJobInTransaction(v, "")
			return err
		})
		if err == nil || err.Error() != "Configure a default resolver pool before apply" {
			t.Fatal(err)
		}
		settingsExec(t, db, "update installation_settings set default_resolver_pool=?", enqueuePool)
		enqueueCall(t, db, "")
	})
	t.Run("initialization error precedes pool", func(t *testing.T) {
		db, _, _ := sqliteSettingsFixture(t)
		err := settingsWriter(db).Write(context.Background(), func(v sqlitedb.WriteValue) error {
			if _, err := v.Conn().ExecContext(v.Context(), `create temp trigger reject_initialization
				before insert on installation_settings begin select raise(ABORT,'initialization failure'); end`); err != nil {
				return err
			}
			_, err := sqliteCreateApplyJobInTransaction(v, "bad")
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "initialization failure") || enqueueCount(t, db, "installation_settings") != 0 {
			t.Fatal(err)
		}
	})
}

func TestSQLiteEnqueueWriterClockBridge(t *testing.T) {
	for _, seconds := range []int64{-210866803201, 9224318016000, 123, -1} {
		t.Run(fmt.Sprint(seconds), func(t *testing.T) {
			db, _ := enqueueFixture(t)
			first := enqueueCall(t, db, "")
			before := enqueueRevisionBytes(t, db)
			stamp := sqlitedb.FiniteTimestamp{Seconds: seconds, Nanos: 987654321}
			var got ApplyResult
			err := sqlitedb.NewWriter(db, func() sqlitedb.FiniteTimestamp { return stamp }).Write(context.Background(), func(v sqlitedb.WriteValue) error {
				var err error
				got, err = sqliteCreateApplyJobInTransaction(v, "")
				return err
			})
			if err != nil || got.RevisionID != first.RevisionID || before != enqueueRevisionBytes(t, db) || !got.Job.CreatedAt.Equal(time.Unix(seconds, 987654321)) || got.Job.CreatedAt.Location() != time.Local {
				t.Fatal(got, err)
			}
			key, _ := sqlitedb.EncodeTimestamp(stamp)
			var raw, updated string
			if err := db.QueryRow("select created_at from apply_jobs where id=?", got.Job.ID).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow("select updated_at from installation_settings").Scan(&updated); err != nil || raw != *key || updated != raw {
				t.Fatal(raw, updated, err)
			}
		})
	}
	for _, seconds := range []int64{math.MinInt64, math.MaxInt64} {
		key, _ := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: seconds})
		if _, err := sqliteEnqueueTime(key); err == nil {
			t.Fatal("unrepresentable calendar clock accepted", seconds)
		}
	}
	for _, key := range []string{"0", "2", "broken"} {
		if _, err := sqliteEnqueueTime(&key); err == nil {
			t.Fatal("invalid/nonfinite clock accepted", key)
		}
	}
	if got, err := sqliteEnqueueTime(nil); got != nil || err != nil {
		t.Fatal(got, err)
	}
}

func TestSQLiteEnqueueReturnedClockScanner(t *testing.T) {
	db, _ := enqueueFixture(t)
	key, _ := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: -1, Nanos: 123456789})
	for _, created := range []any{nil, "0", "2", "broken", *key} {
		t.Run(fmt.Sprint(created), func(t *testing.T) {
			// Actual SQL-returned clocks, including lifecycle values unlike normal
			// queued rows, exercise the shared scanJob destination contract.
			row := db.QueryRow(`select 'job','revision',NULL,'combined','queued','ordinary',
				NULL,NULL,NULL,NULL,NULL,NULL,NULL,'correlation',?,?,NULL,?,NULL,'null',NULL,NULL`, created, *key, *key)
			job, err := scanJob(sqliteEnqueueJobScanner{row})
			if created != *key {
				if err == nil {
					t.Fatal("invalid returned created clock accepted", job)
				}
				return
			}
			want := time.Unix(-1, 123456789)
			if err != nil || !job.CreatedAt.Equal(want) || job.ClaimedAt == nil || !job.ClaimedAt.Equal(want) || job.StartedAt != nil || job.FinishedAt == nil || !job.FinishedAt.Equal(want) || job.ApplyOutput != nil {
				t.Fatal(job, err)
			}
		})
	}
}

func TestSQLiteEnqueueCancellationAndClosedConnection(t *testing.T) {
	db, _ := enqueueFixture(t)
	db.SetMaxOpenConns(1)
	before := snapshotSettingsBytes(t, db)
	ctx, cancel := context.WithCancel(context.Background())
	err := settingsWriter(db).Write(ctx, func(v sqlitedb.WriteValue) error {
		if _, err := sqliteCreateApplyJobInTransaction(v, ""); err != nil {
			return err
		}
		cancel()
		_, err := sqliteCreateApplyJobInTransaction(v, "")
		return err
	})
	if !errors.Is(err, context.Canceled) || before != snapshotSettingsBytes(t, db) || enqueueCount(t, db, "apply_jobs") != 0 {
		t.Fatal(err)
	}
	enqueueCall(t, db, "")
	var retained sqlitedb.WriteValue
	if err := settingsWriter(db).Write(context.Background(), func(v sqlitedb.WriteValue) error { retained = v; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := sqliteCreateApplyJobInTransaction(retained, ""); !errors.Is(err, sql.ErrConnDone) {
		t.Fatal(err)
	}
}
