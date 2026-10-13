package configuration

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zerkc/ProxyCore/apps/api/internal/cluster"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

type ackWriteKeys struct {
	calls        int
	err          error
	id           uuid.UUID
	material     cluster.KeyMaterial
	override     bool
	beforeReturn func(sqlitedb.WriteValue) error
}

func (k *ackWriteKeys) LoadOrCreateInWrite(v sqlitedb.WriteValue, id *uuid.UUID, candidate *cluster.KeyCandidate) (cluster.KeyMaterial, error) {
	k.calls++
	if v.Conn() == nil || v.Context() == nil || id == nil || candidate != nil {
		panic("invalid in-write key request")
	}
	var active int
	if err := v.Conn().QueryRowContext(v.Context(), `select count(*) from cluster_keys where id=? and retired_at is null`, id.String()).Scan(&active); err != nil || active != 1 {
		panic("key request outside active-row boundary")
	}
	if !k.override {
		material, err := cluster.NewKeyMaterial(k.id, bytes.Repeat([]byte{0x5a}, cluster.KEKLength))
		if err != nil {
			return cluster.KeyMaterial{}, err
		}
		k.material = material
	}
	if k.beforeReturn != nil {
		if err := k.beforeReturn(v); err != nil {
			return k.material, err
		}
	}
	return k.material, k.err
}

type ackWriteFixture struct {
	db    *sql.DB
	path  string
	store *SQLiteSnapshotAcknowledgementStore
	keys  *ackWriteKeys
	ack   SnapshotAcknowledgement
}

func newAckWriteFixture(t *testing.T) *ackWriteFixture {
	t.Helper()
	db, path := ackReadFixture(t)
	id := uuid.MustParse(candidateID(10))
	keys := &ackWriteKeys{id: id}
	t.Cleanup(func() { keys.material.Destroy() })
	settingsExec(t, db, `insert into cluster_keys(id,purpose,wrapped_kek,wrapping_key_version,created_at) values (?,'synthetic','synthetic',1,'0')`, id.String())
	settingsExec(t, db, `insert into installation_identity(id,installation_id,node_id,role,leadership_generation,latest_known_generation,cluster_key_id,updated_at) values (?,?,?,'primary',11,11,?,'0')`, installationIDSingleton, ackReadRevision, candidateID(20), id.String())
	settingsExec(t, db, `insert into users(id,username,password_hash,role,created_at,updated_at) values (?,'ack@example.test','synthetic','owner','0','0')`, candidateID(11))
	settingsExec(t, db, `insert into enrollment_tokens(id,token_selector,token_hash,hash_version,created_by_user_id,created_at,expires_at,consumed_at) values (?,'synthetic','synthetic','synthetic',?,'0','2','0')`, candidateID(12), candidateID(11))
	settingsExec(t, db, `insert into enrollment_attempts(id,state,primary_url,local_node_ip,ephemeral_private_key_wrapped,cluster_key_id,created_at,updated_at) values (?,'committed','https://primary.example','192.0.2.1','synthetic',?,'0','0')`, ackReadNode, id.String())
	settingsExec(t, db, `insert into enrollment_grants(attempt_id,token_id,installation_id,node_id,primary_id,primary_generation,node_ephemeral_public_key,sealed_bootstrap_payload,payload_hash,created_at,expires_at) values (?,?,?,?,?,11,'synthetic','synthetic','synthetic','0','2')`, ackReadNode, candidateID(12), ackReadNode, ackReadNode, ackReadRevision)
	ack := SnapshotAcknowledgement{NodeID: ackReadNode, ContentHash: ackReadHash, SnapshotVersion: 7, ReplicationVersion: 9, RevisionID: ackReadRevision, LeadershipGeneration: 11, AppliedAt: time.Unix(100, 123456789), ReceivedAt: time.Unix(101, 987654321)}
	stamp, err := sqliteEnrollmentTokenTime(ack.AppliedAt)
	if err != nil {
		t.Fatal(err)
	}
	settingsExec(t, db, `update config_revisions set source='ordinary',source_primary_id=?,snapshot_content_hash=?,snapshot_version=7,replication_version=9,leadership_generation=11,applied_at=?`, ackReadRevision, ackReadHash, stamp)
	settingsExec(t, db, `insert into apply_jobs(id,revision_id,target,status,correlation_id,created_at,finished_at,source,source_primary_id,snapshot_content_hash,snapshot_version,replication_version,leadership_generation) values (?,?,'nginx','applied','synthetic','0',?,'ordinary',?,?,7,9,11)`, candidateID(13), ackReadRevision, stamp, ackReadRevision, ackReadHash)
	settingsExec(t, db, `insert into applied_snapshots(id,source_primary_id,leadership_generation,snapshot_version,replication_version,content_hash,status,revision_id,apply_job_id,applied_at,snapshot_body) values (?,?,11,7,9,?,'applied',?,?,?,?)`, candidateID(14), ackReadRevision, ackReadHash, ackReadRevision, candidateID(13), stamp, []byte{0, 255})
	writer := sqlitedb.NewWriter(db, func() sqlitedb.FiniteTimestamp { return sqlitedb.FiniteTimestamp{Seconds: 200, Nanos: 456} })
	return &ackWriteFixture{db: db, path: path, store: NewSQLiteSnapshotAcknowledgementStore(db, writer, keys), keys: keys, ack: ack}
}

func ackWriteWant(t *testing.T, f *ackWriteFixture, ack SnapshotAcknowledgement, want error) {
	t.Helper()
	if err := f.store.RecordSnapshotAcknowledgement(context.Background(), ack); !errors.Is(err, want) {
		t.Fatalf("write error=%v; want %v", err, want)
	}
}

func ackWriteReadback(t *testing.T, f *ackWriteFixture, want SnapshotAcknowledgement) {
	t.Helper()
	got, err := f.store.GetSnapshotAcknowledgement(context.Background(), want.NodeID, want.ContentHash)
	clocksEqual := got.AppliedAt.Equal(want.AppliedAt) && got.ReceivedAt.Equal(want.ReceivedAt)
	got.AppliedAt, want.AppliedAt = time.Time{}, time.Time{}
	got.ReceivedAt, want.ReceivedAt = time.Time{}, time.Time{}
	if err != nil || !clocksEqual || got != want {
		t.Fatalf("readback=%#v, %v; want %#v (clocks equal=%v)", got, err, want, clocksEqual)
	}
}

func ackWriteAbsent(t *testing.T, f *ackWriteFixture) {
	t.Helper()
	var count int
	if err := f.db.QueryRow(`select count(*) from node_snapshot_acks`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("ack rows=%d, %v", count, err)
	}
}

func TestSQLiteSnapshotAcknowledgementWriteInsert(t *testing.T) {
	f := newAckWriteFixture(t)
	ackWriteWant(t, f, f.ack, nil)
	ackWriteReadback(t, f, f.ack)
	if f.keys.calls != 1 {
		t.Fatalf("key loads=%d", f.keys.calls)
	}
}

func TestSQLiteSnapshotAcknowledgementWriteDeniedRevoked(t *testing.T) {
	for _, test := range []struct {
		name, sql string
		want      error
	}{
		{"missing grant", `update enrollment_grants set node_id='00000000-0000-0000-0000-000000000099'`, ErrSnapshotAcknowledgementDenied},
		{"revoked credential", `update node_credentials set revoked_at='0'`, ErrSnapshotAcknowledgementRevoked},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newAckWriteFixture(t)
			settingsExec(t, f.db, test.sql)
			ackWriteWant(t, f, f.ack, test.want)
			ackWriteAbsent(t, f)
		})
	}
}

func TestSQLiteSnapshotAcknowledgementWriteValidationPrecedence(t *testing.T) {
	valid := SnapshotAcknowledgement{NodeID: ackReadNode, ContentHash: ackReadHash, SnapshotVersion: 7, ReplicationVersion: 9, RevisionID: ackReadRevision, LeadershipGeneration: 11, AppliedAt: time.Unix(1, 1)}
	tests := []struct {
		name   string
		change func(*SnapshotAcknowledgement)
	}{
		{"zero applied", func(a *SnapshotAcknowledgement) { a.AppliedAt = time.Time{} }},
		{"zero snapshot version", func(a *SnapshotAcknowledgement) { a.SnapshotVersion = 0 }},
		{"negative snapshot version", func(a *SnapshotAcknowledgement) { a.SnapshotVersion = -1 }},
		{"zero replication version", func(a *SnapshotAcknowledgement) { a.ReplicationVersion = 0 }},
		{"negative replication version", func(a *SnapshotAcknowledgement) { a.ReplicationVersion = -1 }},
		{"zero generation", func(a *SnapshotAcknowledgement) { a.LeadershipGeneration = 0 }},
		{"negative generation", func(a *SnapshotAcknowledgement) { a.LeadershipGeneration = -1 }},
	}
	for _, text := range []string{"", "bad", uuid.Nil.String(), strings.ToUpper(ackReadNode), strings.ReplaceAll(ackReadNode, "-", ""), "{" + ackReadNode + "}", "urn:uuid:" + ackReadNode, ackReadNode + "\x00"} {
		tests = append(tests, struct {
			name   string
			change func(*SnapshotAcknowledgement)
		}{"node " + text, func(a *SnapshotAcknowledgement) { a.NodeID = text }}, struct {
			name   string
			change func(*SnapshotAcknowledgement)
		}{"revision " + text, func(a *SnapshotAcknowledgement) { a.RevisionID = text }})
	}
	for _, text := range []string{"", strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("A", 64), strings.Repeat("g", 64), strings.Repeat("a", 63) + "\x00"} {
		tests = append(tests, struct {
			name   string
			change func(*SnapshotAcknowledgement)
		}{"hash " + text, func(a *SnapshotAcknowledgement) { a.ContentHash = text }})
	}
	var typedNil *ackWriteKeys
	stores := []*SQLiteSnapshotAcknowledgementStore{nil, {}, NewSQLiteSnapshotAcknowledgementStore(nil, nil, nil), NewSQLiteSnapshotAcknowledgementStore(nil, nil, typedNil)}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a := valid
			test.change(&a)
			for _, s := range stores {
				if err := s.RecordSnapshotAcknowledgement(context.Background(), a); err != ErrSnapshotAcknowledgementDenied {
					t.Fatalf("validation=%v", err)
				}
			}
		})
	}
	for _, s := range stores {
		if err := s.RecordSnapshotAcknowledgement(nil, valid); err != ErrSnapshotAcknowledgementDenied {
			t.Fatalf("nil context=%v", err)
		}
		if err := s.RecordSnapshotAcknowledgement(context.Background(), valid); err != ErrSnapshotAcknowledgementUnavailable {
			t.Fatalf("missing dependencies=%v", err)
		}
	}
}

func TestSQLiteSnapshotAcknowledgementWriteCredentialJoin(t *testing.T) {
	for _, test := range []struct {
		name, sql string
		want      error
	}{
		{"missing credential", `update node_credentials set node_id='00000000-0000-0000-0000-000000000099'`, ErrSnapshotAcknowledgementDenied},
		{"missing node", `update enrolled_nodes set node_id='00000000-0000-0000-0000-000000000099'`, ErrSnapshotAcknowledgementDenied},
		{"credential id mismatch", `insert into node_credentials(id,node_id,credential_hash,hash_version,created_at) values ('00000000-0000-0000-0000-000000000099','00000000-0000-0000-0000-000000000099','synthetic','synthetic','0'); update enrolled_nodes set credential_id='00000000-0000-0000-0000-000000000099'`, ErrSnapshotAcknowledgementDenied},
		{"grant attempt mismatch", `update enrollment_grants set attempt_id='00000000-0000-0000-0000-000000000099'`, ErrSnapshotAcknowledgementDenied},
		{"grant installation mismatch", `update enrollment_grants set installation_id='00000000-0000-0000-0000-000000000099'`, ErrSnapshotAcknowledgementDenied},
		{"grant primary mismatch", `update enrollment_grants set primary_id='00000000-0000-0000-0000-000000000099'`, ErrSnapshotAcknowledgementDenied},
		{"attempt missing", `update enrollment_attempts set id='00000000-0000-0000-0000-000000000099'`, ErrSnapshotAcknowledgementDenied},
		{"token revoked", `update enrollment_tokens set revoked_at='2'`, ErrSnapshotAcknowledgementRevoked},
		{"node revoked", `update enrolled_nodes set revoked_at='0'`, ErrSnapshotAcknowledgementRevoked},
		{"consumed accepted", `update enrollment_tokens set consumed_at='2'`, nil},
		{"unconsumed accepted", `update enrollment_tokens set consumed_at=null`, nil},
		{"revoked takes precedence", `update node_credentials set revoked_at='0'; update installation_identity set role='node'`, ErrSnapshotAcknowledgementRevoked},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newAckWriteFixture(t)
			settingsExec(t, f.db, test.sql)
			ackWriteWant(t, f, f.ack, test.want)
			if test.want != nil {
				ackWriteAbsent(t, f)
				if f.keys.calls != 0 {
					t.Fatal("loaded keys before credential rejection")
				}
			} else {
				ackWriteReadback(t, f, f.ack)
			}
		})
	}
}

func TestSQLiteSnapshotAcknowledgementWriteIdentityAndActiveKey(t *testing.T) {
	for _, test := range []struct {
		name, sql string
		want      error
	}{
		{"identity missing", `update installation_identity set id='other'`, ErrSnapshotAcknowledgementDenied},
		{"nil installation", `update installation_identity set installation_id='00000000-0000-0000-0000-000000000000'`, ErrSnapshotAcknowledgementDenied},
		{"nil node", `update installation_identity set node_id='00000000-0000-0000-0000-000000000000'`, ErrSnapshotAcknowledgementDenied},
		{"standalone", `update installation_identity set role='standalone-primary'`, ErrSnapshotAcknowledgementDenied},
		{"node", `update installation_identity set role='node'`, ErrSnapshotAcknowledgementDenied},
		{"stale", `update installation_identity set role='stale-primary'`, ErrSnapshotAcknowledgementDenied},
		{"primary with nodes", `update installation_identity set role='primary-with-nodes'`, nil},
		{"zero leadership", `update installation_identity set leadership_generation=0,latest_known_generation=0`, ErrSnapshotAcknowledgementDenied},
		{"negative leadership", `update installation_identity set leadership_generation=-1,latest_known_generation=-1`, ErrSnapshotAcknowledgementDenied},
		{"latest ahead", `update installation_identity set latest_known_generation=12`, ErrSnapshotAcknowledgementDenied},
		{"latest behind", `update installation_identity set latest_known_generation=10`, ErrSnapshotAcknowledgementDenied},
		{"primary mismatch", `update installation_identity set installation_id='00000000-0000-0000-0000-000000000099'`, ErrSnapshotAcknowledgementDenied},
		{"generation mismatch", `update enrollment_grants set primary_generation=12`, ErrSnapshotAcknowledgementDenied},
		{"key null", `update installation_identity set cluster_key_id=null`, ErrSnapshotAcknowledgementDenied},
		{"lineage null", `update enrollment_attempts set cluster_key_id=null`, ErrSnapshotAcknowledgementDenied},
		{"lineage mismatch", `update installation_identity set cluster_key_id='00000000-0000-0000-0000-000000000099'`, ErrSnapshotAcknowledgementDenied},
		{"missing active row", `update installation_identity set cluster_key_id='00000000-0000-0000-0000-000000000099'; update enrollment_attempts set cluster_key_id=null`, ErrSnapshotAcknowledgementDenied},
		{"retired key", `update cluster_keys set retired_at='0'`, ErrSnapshotAcknowledgementDenied},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newAckWriteFixture(t)
			settingsExec(t, f.db, test.sql)
			ackWriteWant(t, f, f.ack, test.want)
			if test.want != nil {
				ackWriteAbsent(t, f)
				if f.keys.calls != 0 {
					t.Fatal("key loader invoked for rejected identity/key")
				}
			} else {
				ackWriteReadback(t, f, f.ack)
			}
		})
	}
	t.Run("missing row never creates", func(t *testing.T) {
		f := newAckWriteFixture(t)
		// Identity has no FK; remove the attempt FK before removing its synthetic key.
		settingsExec(t, f.db, `update enrollment_attempts set cluster_key_id=null; update cluster_keys set id='00000000-0000-0000-0000-000000000099'`)
		// A narrow malformed-data fixture is not needed: add an FK-valid different
		// lineage row and use a trigger to hide active existence only in fault tests.
		ackWriteWant(t, f, f.ack, ErrSnapshotAcknowledgementDenied)
		if f.keys.calls != 0 {
			t.Fatal("missing key created")
		}
	})
}

func TestSQLiteSnapshotAcknowledgementWriteKeyFailures(t *testing.T) {
	for _, name := range []string{"load error", "nil material", "wrong id", "destroyed material", "material with error", "typed nil"} {
		t.Run(name, func(t *testing.T) {
			f := newAckWriteFixture(t)
			switch name {
			case "load error":
				f.keys.err = errors.New("synthetic key failure")
			case "nil material":
				f.keys.override = true
			case "wrong id":
				f.keys.id = uuid.MustParse(candidateID(99))
			case "destroyed material":
				f.keys.override = true
				material, err := cluster.NewKeyMaterial(f.keys.id, make([]byte, cluster.KEKLength))
				if err != nil {
					t.Fatal(err)
				}
				material.Destroy()
				f.keys.material = material
			case "material with error":
				f.keys.err = errors.New("synthetic error with material")
			case "typed nil":
				var keys *ackWriteKeys
				f.store.keys = keys
			}
			ackWriteWant(t, f, f.ack, ErrSnapshotAcknowledgementUnavailable)
			ackWriteAbsent(t, f)
			for _, b := range f.keys.material.CopyBytes() {
				if b != 0 {
					t.Fatal("key bytes not cleared")
				}
			}
		})
	}
}

func TestSQLiteSnapshotAcknowledgementWriteProofPredicates(t *testing.T) {
	other := candidateID(99)
	tests := []struct {
		name, table, column string
		value               any
	}{
		{"body null", "applied_snapshots", "snapshot_body", nil},
		{"body empty", "applied_snapshots", "snapshot_body", []byte{}},
		{"hash", "applied_snapshots", "content_hash", strings.Repeat("b", 64)},
		{"status", "applied_snapshots", "status", "pending"},
		{"discarded", "applied_snapshots", "discarded_at", "0"},
		{"primary", "applied_snapshots", "source_primary_id", other},
		{"generation", "applied_snapshots", "leadership_generation", 12},
		{"revision missing", "applied_snapshots", "revision_id", nil},
		{"job missing", "applied_snapshots", "apply_job_id", nil},
		{"applied time exact nanos", "applied_snapshots", "applied_at", ackReadStamp(t, 100, 123456788)},
	}
	for _, table := range []string{"config_revisions", "apply_jobs"} {
		tests = append(tests, struct {
			name, table, column string
			value               any
		}{"source " + table, table, "source", "import"}, struct {
			name, table, column string
			value               any
		}{"primary " + table, table, "source_primary_id", other}, struct {
			name, table, column string
			value               any
		}{"primary null " + table, table, "source_primary_id", nil}, struct {
			name, table, column string
			value               any
		}{"node " + table, table, "source_node_id", other}, struct {
			name, table, column string
			value               any
		}{"source revision " + table, table, "source_revision_id", other}, struct {
			name, table, column string
			value               any
		}{"hash " + table, table, "snapshot_content_hash", strings.Repeat("b", 64)})
		for _, column := range []string{"snapshot_content_hash", "snapshot_version", "replication_version", "leadership_generation"} {
			tests = append(tests, struct {
				name, table, column string
				value               any
			}{"null " + table + " " + column, table, column, nil})
			if column != "snapshot_content_hash" {
				tests = append(tests, struct {
					name, table, column string
					value               any
				}{"mismatch " + table + " " + column, table, column, 12})
			}
		}
	}
	tests = append(tests, struct {
		name, table, column string
		value               any
	}{"revision unapplied", "config_revisions", "applied_at", nil}, struct {
		name, table, column string
		value               any
	}{"job unfinished", "apply_jobs", "finished_at", nil}, struct {
		name, table, column string
		value               any
	}{"job status", "apply_jobs", "status", "failed"})
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newAckWriteFixture(t)
			settingsExec(t, f.db, "update "+test.table+" set "+test.column+"=?", test.value)
			ackWriteWant(t, f, f.ack, ErrSnapshotAcknowledgementDenied)
			ackWriteAbsent(t, f)
		})
	}
	t.Run("job revision mismatch", func(t *testing.T) {
		f := newAckWriteFixture(t)
		settingsExec(t, f.db, `insert into config_revisions(id,revision_number,checksum,snapshot,created_at) values (?,2,'synthetic-job-mismatch','{}','0')`, other)
		settingsExec(t, f.db, `update apply_jobs set revision_id=?`, other)
		ackWriteWant(t, f, f.ack, ErrSnapshotAcknowledgementDenied)
	})
	for _, target := range []string{"combined", "nginx", "coredns", "certificate"} {
		t.Run("target "+target, func(t *testing.T) {
			f := newAckWriteFixture(t)
			settingsExec(t, f.db, `update apply_jobs set target=?`, target)
			ackWriteWant(t, f, f.ack, nil)
		})
	}
}

func TestSQLiteSnapshotAcknowledgementWriteTupleMismatch(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*SnapshotAcknowledgement)
	}{
		{"snapshot", func(a *SnapshotAcknowledgement) { a.SnapshotVersion++ }},
		{"replication", func(a *SnapshotAcknowledgement) { a.ReplicationVersion++ }},
		{"revision", func(a *SnapshotAcknowledgement) { a.RevisionID = candidateID(99) }},
		{"generation", func(a *SnapshotAcknowledgement) { a.LeadershipGeneration++ }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newAckWriteFixture(t)
			a := f.ack
			test.change(&a)
			ackWriteWant(t, f, a, ErrSnapshotAcknowledgementDenied)
			ackWriteAbsent(t, f)
		})
	}
}

func TestSQLiteSnapshotAcknowledgementWriteOverwriteCompositeReopen(t *testing.T) {
	f := newAckWriteFixture(t)
	ackWriteWant(t, f, f.ack, nil)
	// Historical values can move backwards; neither timestamp is first-write-only.
	second := f.ack
	second.SnapshotVersion = 3
	second.ReplicationVersion = 4
	second.LeadershipGeneration = 8
	settingsExec(t, f.db, `update installation_identity set leadership_generation=8,latest_known_generation=8; update enrollment_grants set primary_generation=8`)
	second.RevisionID = candidateID(30)
	second.AppliedAt = time.Unix(-1, 999999999)
	second.ReceivedAt = time.Unix(-10, 987654321)
	stamp, _ := sqliteEnrollmentTokenTime(second.AppliedAt)
	settingsExec(t, f.db, `insert into config_revisions(id,revision_number,checksum,snapshot,created_at,source,source_primary_id,snapshot_content_hash,snapshot_version,replication_version,leadership_generation,applied_at) values (?,2,'synthetic-overwrite','{}','0','ordinary',?,?,3,4,8,?)`, second.RevisionID, ackReadRevision, ackReadHash, stamp)
	settingsExec(t, f.db, `update apply_jobs set revision_id=?,snapshot_version=3,replication_version=4,leadership_generation=8`, second.RevisionID)
	settingsExec(t, f.db, `update applied_snapshots set revision_id=?,snapshot_version=3,replication_version=4,leadership_generation=8,applied_at=?`, second.RevisionID, stamp)
	ackWriteWant(t, f, second, nil)
	ackWriteReadback(t, f, second)
	// Independent composite key remains untouched.
	sibling := ackReadInsert(t, f.db, f.ack.NodeID, strings.Repeat("b", 64))
	ackWriteWant(t, f, second, nil)
	ackWriteReadback(t, f, sibling)
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlitedb.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	f.store = NewSQLiteSnapshotAcknowledgementStore(reopened, sqlitedb.NewWriter(reopened, nil), f.keys)
	ackWriteReadback(t, f, second)
	ackWriteReadback(t, f, sibling)
}

func TestSQLiteSnapshotAcknowledgementWriteCallerClock(t *testing.T) {
	for _, received := range []time.Time{time.Time{}, time.Unix(-123, 123456789), time.Date(2000, 1, 1, 0, 0, 0, 1, time.FixedZone("synthetic", 3600))} {
		t.Run(received.String(), func(t *testing.T) {
			f := newAckWriteFixture(t)
			f.ack.ReceivedAt = received
			ackWriteWant(t, f, f.ack, nil)
			want := f.ack
			if received.IsZero() {
				want.ReceivedAt = time.Unix(200, 456)
			} else {
				want.ReceivedAt = received.UTC()
			}
			ackWriteReadback(t, f, want)
		})
	}
}

func TestSQLiteSnapshotAcknowledgementWriteUnavailable(t *testing.T) {
	for _, name := range []string{"cancelled", "closed", "unmigrated", "no writer", "nil keys"} {
		t.Run(name, func(t *testing.T) {
			f := newAckWriteFixture(t)
			ctx := context.Background()
			switch name {
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "closed":
				if err := f.db.Close(); err != nil {
					t.Fatal(err)
				}
			case "unmigrated":
				db, err := sqlitedb.Open(f.path + ".empty")
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				f.store = NewSQLiteSnapshotAcknowledgementStore(db, sqlitedb.NewWriter(db, nil), f.keys)
			case "no writer":
				f.store.writer = nil
			case "nil keys":
				f.store.keys = nil
			}
			if err := f.store.RecordSnapshotAcknowledgement(ctx, f.ack); err != ErrSnapshotAcknowledgementUnavailable {
				t.Fatalf("unavailable=%v", err)
			}
		})
	}
}

// The wrapper delegates to a real migrated SQLite file, injecting only one
// named boundary. All ordinary SQL, transaction rollback and FK checks remain
// real; malformed row values cannot otherwise be produced by STRICT tables.
type ackWriteFault struct {
	execMatch       string
	queryMatch      string
	scanMatch       string
	scanColumn      int
	scanValue       driver.Value
	nextError       bool
	noRowsMatch     string
	proofRows       [][]driver.Value
	begin           chan struct{}
	beginOnce       sync.Once
	begins, commits int
}

type ackWriteConnector struct {
	base  driver.Driver
	path  string
	fault *ackWriteFault
}

func (c ackWriteConnector) Driver() driver.Driver { return c.base }
func (c ackWriteConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.base.Open(c.path)
	if err != nil {
		return nil, err
	}
	executor, ok := conn.(driver.ExecerContext)
	if !ok {
		conn.Close()
		return nil, errors.New("fixture driver lacks context exec")
	}
	for _, query := range []string{`pragma foreign_keys=on`, `pragma busy_timeout=5000`} {
		if _, err := executor.ExecContext(context.Background(), query, nil); err != nil {
			conn.Close()
			return nil, err
		}
	}
	return &ackWriteFaultConn{Conn: conn, fault: c.fault}, nil
}

type ackWriteFaultConn struct {
	driver.Conn
	fault *ackWriteFault
}

func (c *ackWriteFaultConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	if q == "BEGIN IMMEDIATE" {
		c.fault.begins++
		if c.fault.begin != nil {
			c.fault.beginOnce.Do(func() { close(c.fault.begin) })
		}
	}
	if q == "COMMIT" {
		c.fault.commits++
	}
	if c.fault.execMatch != "" && strings.Contains(q, c.fault.execMatch) {
		return nil, errors.New("synthetic exec failure")
	}
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, q, args)
}
func (c *ackWriteFaultConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if c.fault.queryMatch != "" && strings.Contains(q, c.fault.queryMatch) {
		return nil, errors.New("synthetic query failure")
	}
	if c.fault.proofRows != nil && strings.Contains(q, "from applied_snapshots") {
		if !strings.Contains(q, "order by a.applied_at desc,a.id desc") {
			return nil, errors.New("unordered proof candidates")
		}
		return &ackWriteProofRows{values: c.fault.proofRows}, nil
	}
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
	if err != nil {
		return nil, err
	}
	if c.fault.noRowsMatch != "" && strings.Contains(q, c.fault.noRowsMatch) {
		return &ackWriteEmptyRows{Rows: rows}, nil
	}
	if c.fault.scanMatch != "" && strings.Contains(q, c.fault.scanMatch) {
		return &ackWriteFaultRows{Rows: rows, fault: c.fault}, nil
	}
	return rows, nil
}

type ackWriteEmptyRows struct{ driver.Rows }

func (r *ackWriteEmptyRows) Next([]driver.Value) error { return io.EOF }

type ackWriteProofRows struct {
	values [][]driver.Value
	index  int
}

func (*ackWriteProofRows) Columns() []string {
	return []string{"primary", "generation", "snapshot", "replication", "revision", "applied"}
}
func (*ackWriteProofRows) Close() error { return nil }
func (r *ackWriteProofRows) Next(dest []driver.Value) error {
	if r.index == len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.index])
	r.index++
	return nil
}

type ackWriteFaultRows struct {
	driver.Rows
	fault *ackWriteFault
}

func (r *ackWriteFaultRows) Next(dest []driver.Value) error {
	err := r.Rows.Next(dest)
	if err != nil {
		return err
	}
	if r.fault.nextError {
		return errors.New("synthetic rows failure")
	}
	dest[r.fault.scanColumn] = r.fault.scanValue
	return nil
}
func ackWriteFaultDB(t *testing.T, f *ackWriteFixture, fault *ackWriteFault) *sql.DB {
	t.Helper()
	db := sql.OpenDB(ackWriteConnector{base: f.db.Driver(), path: f.path, fault: fault})
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func TestSQLiteSnapshotAcknowledgementWriteFaultsRollback(t *testing.T) {
	tests := []struct {
		name  string
		fault ackWriteFault
		want  error
	}{
		{"begin", ackWriteFault{execMatch: "BEGIN IMMEDIATE"}, ErrSnapshotAcknowledgementUnavailable},
		{"commit", ackWriteFault{execMatch: "COMMIT"}, ErrSnapshotAcknowledgementUnavailable},
		{"upsert", ackWriteFault{execMatch: "insert into node_snapshot_acks"}, ErrSnapshotAcknowledgementUnavailable},
		{"credential query", ackWriteFault{queryMatch: "from node_credentials"}, ErrSnapshotAcknowledgementUnavailable},
		{"identity query", ackWriteFault{queryMatch: "from installation_identity"}, ErrSnapshotAcknowledgementUnavailable},
		{"active query", ackWriteFault{queryMatch: "select exists"}, ErrSnapshotAcknowledgementUnavailable},
		{"proof query", ackWriteFault{queryMatch: "from applied_snapshots"}, ErrSnapshotAcknowledgementUnavailable},
		{"credential scan", ackWriteFault{scanMatch: "from node_credentials", scanColumn: 9, scanValue: "bad integer"}, ErrSnapshotAcknowledgementUnavailable},
		{"identity scan", ackWriteFault{scanMatch: "from installation_identity", scanColumn: 3, scanValue: "bad integer"}, ErrSnapshotAcknowledgementUnavailable},
		{"identity noncanonical", ackWriteFault{scanMatch: "from installation_identity", scanColumn: 0, scanValue: strings.ToUpper(ackReadNode)}, ErrSnapshotAcknowledgementDenied},
		{"identity malformed key", ackWriteFault{scanMatch: "from installation_identity", scanColumn: 5, scanValue: "malformed"}, ErrSnapshotAcknowledgementUnavailable},
		{"active scan", ackWriteFault{scanMatch: "select exists", scanColumn: 0, scanValue: "not boolean"}, ErrSnapshotAcknowledgementUnavailable},
		{"active missing", ackWriteFault{scanMatch: "select exists", scanColumn: 0, scanValue: int64(0)}, ErrSnapshotAcknowledgementDenied},
		{"proof scan", ackWriteFault{scanMatch: "from applied_snapshots", scanColumn: 1, scanValue: "bad integer"}, ErrSnapshotAcknowledgementUnavailable},
		{"proof malformed clock", ackWriteFault{scanMatch: "from applied_snapshots", scanColumn: 5, scanValue: "bad clock"}, ErrSnapshotAcknowledgementUnavailable},
		{"proof infinity", ackWriteFault{scanMatch: "from applied_snapshots", scanColumn: 5, scanValue: "2"}, ErrSnapshotAcknowledgementUnavailable},
		{"proof rows", ackWriteFault{scanMatch: "from applied_snapshots", nextError: true}, ErrSnapshotAcknowledgementUnavailable},
		{"proof nil revision", ackWriteFault{scanMatch: "from applied_snapshots", scanColumn: 4, scanValue: uuid.Nil.String()}, ErrSnapshotAcknowledgementDenied},
		{"proof noncanonical revision", ackWriteFault{scanMatch: "from applied_snapshots", scanColumn: 4, scanValue: strings.ToUpper(ackReadNode)}, ErrSnapshotAcknowledgementDenied},
		{"proof null clock", ackWriteFault{scanMatch: "from applied_snapshots", scanColumn: 5, scanValue: nil}, ErrSnapshotAcknowledgementUnavailable},
		{"missing token join", ackWriteFault{noRowsMatch: "from node_credentials"}, ErrSnapshotAcknowledgementDenied},
		{"proof zero version", ackWriteFault{scanMatch: "from applied_snapshots", scanColumn: 2, scanValue: int64(0)}, ErrSnapshotAcknowledgementDenied},
		{"proof negative replication", ackWriteFault{scanMatch: "from applied_snapshots", scanColumn: 3, scanValue: int64(-1)}, ErrSnapshotAcknowledgementDenied},
	}
	for i := range tests {
		test := &tests[i]
		t.Run(test.name, func(t *testing.T) {
			f := newAckWriteFixture(t)
			old := ackReadInsert(t, f.db, f.ack.NodeID, f.ack.ContentHash)
			db := ackWriteFaultDB(t, f, &test.fault)
			f.store = NewSQLiteSnapshotAcknowledgementStore(db, sqlitedb.NewWriter(db, nil), f.keys)
			ackWriteWant(t, f, f.ack, test.want)
			// Read through the original real handle so the fault does not mask rollback.
			f.store.SQLiteSnapshotAcknowledgementReadStore = NewSQLiteSnapshotAcknowledgementReadStore(f.db)
			ackWriteReadback(t, f, old)
			if test.fault.begins != 1 {
				t.Fatalf("writer invocations=%d", test.fault.begins)
			}
			if test.want == ErrSnapshotAcknowledgementDenied && f.keys.calls != 0 && test.name == "active missing" {
				t.Fatal("missing active row invoked loader")
			}
		})
	}
}

func TestSQLiteSnapshotAcknowledgementWriteOneTransaction(t *testing.T) {
	f := newAckWriteFixture(t)
	fault := &ackWriteFault{}
	db := ackWriteFaultDB(t, f, fault)
	clocks := 0
	writer := sqlitedb.NewWriter(db, func() sqlitedb.FiniteTimestamp { clocks++; return sqlitedb.FiniteTimestamp{Seconds: 200, Nanos: 456} })
	f.store = NewSQLiteSnapshotAcknowledgementStore(db, writer, f.keys)
	ackWriteWant(t, f, f.ack, nil)
	if fault.begins != 1 || fault.commits != 1 || clocks != 1 || f.keys.calls != 1 {
		t.Fatalf("begin/commit/clock/key=%d/%d/%d/%d", fault.begins, fault.commits, clocks, f.keys.calls)
	}
	ackWriteReadback(t, f, f.ack)
}

func TestSQLiteSnapshotAcknowledgementWriteHeldWriterRevalidation(t *testing.T) {
	for _, test := range []struct {
		name, mutation string
		want           error
	}{
		{"revocation", `update enrollment_tokens set revoked_at='0'`, ErrSnapshotAcknowledgementRevoked},
		{"identity", `update installation_identity set latest_known_generation=12`, ErrSnapshotAcknowledgementDenied},
		{"key retirement", `update cluster_keys set retired_at='0'`, ErrSnapshotAcknowledgementDenied},
		{"proof change", `update applied_snapshots set snapshot_body=null`, ErrSnapshotAcknowledgementDenied},
		{"zero received clock", "", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newAckWriteFixture(t)
			fault := &ackWriteFault{begin: make(chan struct{})}
			db := ackWriteFaultDB(t, f, fault)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			held := make(chan sqlitedb.WriteValue, 1)
			release := make(chan struct{})
			holderDone := make(chan error, 1)
			go func() {
				holderDone <- f.store.writer.Write(ctx, func(v sqlitedb.WriteValue) error { held <- v; <-release; return nil })
			}()
			v := <-held
			var once sync.Once
			unlock := func() { once.Do(func() { close(release) }) }
			defer unlock()
			// The contender's clock samples a value changed while the first writer holds
			// the lock. Channel closure synchronizes the clock read after release.
			var clockSeconds atomic.Int64
			clockSeconds.Store(200)
			writer := sqlitedb.NewWriter(db, func() sqlitedb.FiniteTimestamp {
				return sqlitedb.FiniteTimestamp{Seconds: clockSeconds.Load(), Nanos: 789}
			})
			store := NewSQLiteSnapshotAcknowledgementStore(db, writer, f.keys)
			ack := f.ack
			ack.ReceivedAt = time.Time{}
			contenderDone := make(chan error, 1)
			go func() { contenderDone <- store.RecordSnapshotAcknowledgement(ctx, ack) }()
			select {
			case <-fault.begin:
			case <-ctx.Done():
				t.Fatal("contender did not reach BEGIN")
			}
			// No sleeps or pre-call goroutine signal: the instrumented connection has
			// entered its BEGIN boundary, while the other handle owns the actual lock.
			select {
			case err := <-contenderDone:
				t.Fatalf("completed while held: %v", err)
			default:
			}
			if test.mutation != "" {
				if _, err := v.Conn().ExecContext(v.Context(), test.mutation); err != nil {
					t.Fatal(err)
				}
			}
			clockSeconds.Store(300)
			unlock()
			if err := <-holderDone; err != nil {
				t.Fatal(err)
			}
			if err := <-contenderDone; !errors.Is(err, test.want) {
				t.Fatalf("after wait=%v; want %v", err, test.want)
			}
			if test.want == nil {
				ack.ReceivedAt = time.Unix(300, 789)
				f.store = store
				ackWriteReadback(t, f, ack)
			} else {
				ackWriteAbsent(t, f)
				wantLoads := 0
				if test.name == "proof change" {
					wantLoads = 1
				}
				if f.keys.calls != wantLoads {
					t.Fatalf("revalidation key loads=%d; want %d", f.keys.calls, wantLoads)
				}
			}
		})
	}
}

func TestSQLiteSnapshotAcknowledgementWriteCandidateTraversal(t *testing.T) {
	for _, test := range []struct {
		name string
		rows func(*testing.T) [][]driver.Value
		want error
	}{
		{"skip newer nonmatching time", func(t *testing.T) [][]driver.Value {
			return [][]driver.Value{
				{ackReadRevision, int64(11), int64(8), int64(9), candidateID(99), ackReadStamp(t, 101, 123456789)},
				{ackReadRevision, int64(11), int64(7), int64(9), ackReadRevision, ackReadStamp(t, 100, 123456789)},
			}
		}, nil},
		{"first matching id wins", func(t *testing.T) [][]driver.Value {
			return [][]driver.Value{
				{ackReadRevision, int64(11), int64(8), int64(9), candidateID(99), ackReadStamp(t, 100, 123456789)},
				{ackReadRevision, int64(11), int64(7), int64(9), ackReadRevision, ackReadStamp(t, 100, 123456789)},
			}
		}, ErrSnapshotAcknowledgementDenied},
		{"only exact nanoseconds", func(t *testing.T) [][]driver.Value {
			return [][]driver.Value{
				{ackReadRevision, int64(11), int64(7), int64(9), ackReadRevision, ackReadStamp(t, 100, 123456790)},
				{ackReadRevision, int64(11), int64(7), int64(9), ackReadRevision, ackReadStamp(t, 100, 123456788)},
			}
		}, ErrSnapshotAcknowledgementDenied},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newAckWriteFixture(t)
			// The migrated schema has a UNIQUE content_hash index. Narrow driver rows
			// exercise PG's retained traversal contract without weakening that schema.
			fault := &ackWriteFault{proofRows: test.rows(t)}
			db := ackWriteFaultDB(t, f, fault)
			f.store = NewSQLiteSnapshotAcknowledgementStore(db, sqlitedb.NewWriter(db, nil), f.keys)
			ackWriteWant(t, f, f.ack, test.want)
			if test.want != nil {
				ackWriteAbsent(t, f)
			} else {
				ackWriteReadback(t, f, f.ack)
			}
		})
	}
}

func TestSQLiteSnapshotAcknowledgementWriteRollbackAll(t *testing.T) {
	for _, name := range []string{"key side effect", "insert commit failure", "update commit failure", "trigger upsert failure"} {
		t.Run(name, func(t *testing.T) {
			f := newAckWriteFixture(t)
			var old SnapshotAcknowledgement
			if name == "update commit failure" || name == "trigger upsert failure" {
				old = ackReadInsert(t, f.db, f.ack.NodeID, f.ack.ContentHash)
			}
			switch name {
			case "key side effect":
				f.keys.beforeReturn = func(v sqlitedb.WriteValue) error {
					if _, err := v.Conn().ExecContext(v.Context(), `update installation_identity set updated_at=?`, v.Timestamp()); err != nil {
						return err
					}
					return errors.New("synthetic key failure after write")
				}
			case "trigger upsert failure":
				settingsExec(t, f.db, `create trigger ack_write_reject before update on node_snapshot_acks begin select raise(abort,'synthetic upsert rejection'); end`)
			default:
				fault := &ackWriteFault{execMatch: "COMMIT"}
				db := ackWriteFaultDB(t, f, fault)
				f.store = NewSQLiteSnapshotAcknowledgementStore(db, sqlitedb.NewWriter(db, nil), f.keys)
			}
			ackWriteWant(t, f, f.ack, ErrSnapshotAcknowledgementUnavailable)
			f.store.SQLiteSnapshotAcknowledgementReadStore = NewSQLiteSnapshotAcknowledgementReadStore(f.db)
			if old.NodeID == "" {
				ackWriteAbsent(t, f)
			} else {
				ackWriteReadback(t, f, old)
			}
			var updated string
			if err := f.db.QueryRow(`select updated_at from installation_identity`).Scan(&updated); err != nil || updated != "0" {
				t.Fatalf("key side effect escaped rollback: %q, %v", updated, err)
			}
			for _, b := range f.keys.material.CopyBytes() {
				if b != 0 {
					t.Fatal("material not destroyed on rollback")
				}
			}
		})
	}
}

func TestSQLiteSnapshotAcknowledgementWriteRealKeyStore(t *testing.T) {
	f := newAckWriteFixture(t)
	master := base64.StdEncoding.EncodeToString(make([]byte, 32))
	keys := cluster.NewSQLiteKeyStore(f.db, f.store.writer, master)
	candidate, err := keys.NewKeyCandidate()
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Destroy()
	// Retire the synthetic row so the public API creates a real wrapped key.
	settingsExec(t, f.db, `update cluster_keys set retired_at='0'`)
	var id uuid.UUID
	err = f.store.writer.Write(context.Background(), func(v sqlitedb.WriteValue) error {
		material, err := keys.LoadOrCreateInWrite(v, nil, candidate)
		defer material.Destroy()
		id = material.ID
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	settingsExec(t, f.db, `update installation_identity set cluster_key_id=?`, id.String())
	settingsExec(t, f.db, `update enrollment_attempts set cluster_key_id=?`, id.String())
	f.store.keys = keys
	ackWriteWant(t, f, f.ack, nil)
	ackWriteReadback(t, f, f.ack)
	f.ack.ReceivedAt = time.Unix(102, 1)
	f.store.keys = cluster.NewSQLiteKeyStore(f.db, f.store.writer, base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32))))
	ackWriteWant(t, f, f.ack, ErrSnapshotAcknowledgementUnavailable)
	old := f.ack
	old.ReceivedAt = time.Unix(101, 987654321)
	ackWriteReadback(t, f, old)
}
