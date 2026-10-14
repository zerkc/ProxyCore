package configuration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/cluster"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

type sqlitePublicationFixture struct {
	db         *sql.DB
	writer     *sqlitedb.Writer
	store      *SQLiteSnapshotPublicationStore
	master     string
	selector   string
	credential string
	primaryID  string
	nodeID     string
	peerID     string
	clusterKey uuid.UUID
	identity   SnapshotPublicationIdentityRecord
	revisions  int
}

func newSQLitePublicationFixture(t *testing.T) *sqlitePublicationFixture {
	t.Helper()
	db, _, _ := sqliteSettingsFixture(t)
	writer := settingsWriter(db)
	f := &sqlitePublicationFixture{
		db: db, writer: writer, selector: strings.Repeat("ab", 16),
		credential: uuid.NewString(), primaryID: uuid.NewString(), nodeID: uuid.NewString(), peerID: uuid.NewString(),
		master: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x37}, 32)),
	}
	keys := cluster.NewSQLiteKeyStore(db, writer, f.master)
	material, err := keys.LoadOrCreate(context.Background(), nil)
	if err != nil {
		t.Fatalf("create cluster key: %v", err)
	}
	f.clusterKey = material.ID
	material.Destroy()
	f.store = NewSQLiteSnapshotPublicationStore(writer, keys)
	owner := uuid.NewString()
	settingsExec(t, db, `insert into users(id,username,password_hash,role,created_at,updated_at) values(?, 'synthetic-owner','dummy','owner','0','0')`, owner)
	settingsExec(t, db, `insert into installation_identity(id,installation_id,node_id,role,leadership_generation,latest_known_generation,cluster_key_id,updated_at) values(?,?,?,'primary',7,7,?,'0')`,
		installationID, f.primaryID, f.nodeID, f.clusterKey.String())
	settingsExec(t, db, `insert into enrollment_tokens(id,token_selector,token_hash,hash_version,created_by_user_id,created_at,expires_at) values(?,?,'stored-hash',?,?,?,?)`,
		uuid.NewString(), f.selector, EnrollmentTokenHashVersion, owner,
		tokenKey(t, time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)), tokenKey(t, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)))
	settingsExec(t, db, `insert into node_credentials(id,node_id,credential_hash,hash_version,created_at) values(?,?,'synthetic','fixture','0')`, f.credential, f.peerID)
	settingsExec(t, db, `insert into enrolled_nodes(node_id,installation_id,primary_id,credential_id,enrolled_at,created_by_attempt_id) values(?,?,?,?,'0',?)`,
		f.peerID, f.peerID, f.primaryID, f.credential, uuid.NewString())
	keyID := f.clusterKey
	f.identity = SnapshotPublicationIdentityRecord{
		InstallationID: f.primaryID, NodeID: f.nodeID, Role: domain.TopologyRolePrimary,
		LeadershipGeneration: 7, LatestKnownGeneration: 7, ClusterKeyID: &keyID,
	}
	return f
}

// insertSnapshot stores one revision, job and applied snapshot. A non-terminal
// candidate has a queued job and a pending snapshot, so its proof is incomplete.
func (f *sqlitePublicationFixture) insertSnapshot(t *testing.T, body []byte, hash string, terminal bool) {
	t.Helper()
	f.revisions++
	revisionID, jobID, snapshotID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	created := tokenKey(t, time.Date(2026, 2, 3, 4, 5, f.revisions, 0, time.UTC))
	status, jobStatus := "pending", "queued"
	var finished, revisionApplied any
	if terminal {
		status, jobStatus = "applied", "applied"
		finished, revisionApplied = created, created
	}
	settingsExec(t, f.db, `insert into config_revisions(id,revision_number,checksum,snapshot,created_at,applied_at,source,source_primary_id,snapshot_content_hash,snapshot_version,replication_version,leadership_generation) values(?,?,?,'{}',?,?,'ordinary',?,?,1,1,7)`,
		revisionID, f.revisions, uuid.NewString(), created, revisionApplied, f.primaryID, hash)
	settingsExec(t, f.db, `insert into apply_jobs(id,revision_id,target,status,correlation_id,created_at,finished_at,source,source_primary_id,snapshot_content_hash,snapshot_version,replication_version,leadership_generation) values(?,?,'combined',?,?,?,?,'ordinary',?,?,1,1,7)`,
		jobID, revisionID, jobStatus, uuid.NewString(), created, finished, f.primaryID, hash)
	var stored any
	if body != nil {
		stored = body
	}
	settingsExec(t, f.db, `insert into applied_snapshots(id,source_primary_id,leadership_generation,snapshot_version,replication_version,content_hash,snapshot_body,revision_id,status,apply_job_id,applied_at) values(?,?,7,1,1,?,?,?,?,?,?)`,
		snapshotID, f.primaryID, hash, stored, revisionID, status, jobID, created)
}

func (f *sqlitePublicationFixture) insertRawBody(t *testing.T, body []byte, terminal bool) {
	t.Helper()
	sum := sha256.Sum256(append([]byte(nil), body...))
	f.insertSnapshot(t, body, hex.EncodeToString(sum[:]), terminal)
}

func (f *sqlitePublicationFixture) read(t *testing.T, store *SQLiteSnapshotPublicationStore, identity SnapshotPublicationIdentityRecord, selector ...string) ([]byte, error) {
	t.Helper()
	chosen := f.selector
	if len(selector) > 0 {
		chosen = selector[0]
	}
	return store.ReadSnapshotPublicationByToken(context.Background(), chosen, identity)
}

func TestSQLiteSnapshotPublicationByTokenHappyPath(t *testing.T) {
	f := newSQLitePublicationFixture(t)
	body := []byte(`{"published":"canonical"}`)
	f.insertRawBody(t, body, true)
	got, err := f.read(t, f.store, f.identity)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("read body=%q err=%v want=%q", got, err, body)
	}
	got[0] = 'X'
	second, err := f.read(t, f.store, f.identity)
	if err != nil || !bytes.Equal(second, body) {
		t.Fatalf("defensive copy body=%q err=%v want=%q", second, err, body)
	}
}

func TestSQLiteSnapshotPublicationByTokenDoesNotConsumeToken(t *testing.T) {
	f := newSQLitePublicationFixture(t)
	f.insertRawBody(t, []byte(`{"a":1}`), true)
	if _, err := f.read(t, f.store, f.identity); err != nil {
		t.Fatal(err)
	}
	var consumed *string
	if err := f.db.QueryRow(`select consumed_at from enrollment_tokens where token_selector=?`, f.selector).Scan(&consumed); err != nil || consumed != nil {
		t.Fatalf("consumed=%v err=%v", consumed, err)
	}
}

func TestSQLiteSnapshotPublicationByTokenSelectsNewestPublishable(t *testing.T) {
	f := newSQLitePublicationFixture(t)
	f.insertRawBody(t, []byte(`{"n":"old"}`), true)
	f.insertRawBody(t, []byte(`{"n":"new"}`), true)
	got, err := f.read(t, f.store, f.identity)
	if err != nil || string(got) != `{"n":"new"}` {
		t.Fatalf("body=%q err=%v", got, err)
	}
}

func TestSQLiteSnapshotPublicationByTokenNoPublishableSnapshot(t *testing.T) {
	for _, test := range []struct {
		name     string
		body     []byte
		terminal bool
	}{
		{name: "legacy null body", terminal: true},
		{name: "incomplete terminal proof", body: []byte(`{"published":true}`), terminal: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newSQLitePublicationFixture(t)
			if test.body == nil {
				f.insertSnapshot(t, nil, strings.Repeat("c", 64), test.terminal)
			} else {
				f.insertRawBody(t, test.body, test.terminal)
			}
			if _, err := f.read(t, f.store, f.identity); !errors.Is(err, ErrNoPublishableSnapshot) {
				t.Fatalf("error=%v want ErrNoPublishableSnapshot", err)
			}
		})
	}
	f := newSQLitePublicationFixture(t)
	if _, err := f.read(t, f.store, f.identity); !errors.Is(err, ErrNoPublishableSnapshot) {
		t.Fatalf("empty table error=%v want ErrNoPublishableSnapshot", err)
	}
}

func TestSQLiteSnapshotPublicationByTokenRejectsTokenLifecycle(t *testing.T) {
	for _, test := range []struct {
		name  string
		want  error
		query string
	}{
		{name: "selector not found", want: ErrSnapshotByTokenUnauthenticated},
		{name: "hash version mismatch", want: ErrSnapshotByTokenUnauthenticated, query: `update enrollment_tokens set hash_version='sha256-other'`},
		{name: "expired", want: ErrSnapshotByTokenUnauthenticated, query: `update enrollment_tokens set expires_at=?`},
		{name: "consumed", want: ErrSnapshotByTokenGone, query: `update enrollment_tokens set consumed_at=?`},
		{name: "revoked", want: ErrSnapshotByTokenGone, query: `update enrollment_tokens set revoked_at=?`},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newSQLitePublicationFixture(t)
			f.insertRawBody(t, []byte(`{"a":1}`), true)
			selector := f.selector
			if test.query == "" {
				selector = strings.Repeat("cd", 16)
			} else if strings.Contains(test.query, "?") {
				settingsExec(t, f.db, test.query, tokenKey(t, time.Date(2020, 1, 3, 0, 0, 0, 0, time.UTC)))
			} else {
				settingsExec(t, f.db, test.query)
			}
			if _, err := f.read(t, f.store, f.identity, selector); !errors.Is(err, test.want) {
				t.Fatalf("error=%v want %v", err, test.want)
			}
		})
	}
	t.Run("empty selector", func(t *testing.T) {
		f := newSQLitePublicationFixture(t)
		if _, err := f.read(t, f.store, f.identity, ""); !errors.Is(err, ErrSnapshotByTokenUnauthenticated) {
			t.Fatalf("error=%v", err)
		}
	})
}

func TestSQLiteSnapshotPublicationByTokenRejectsIdentityAndKeyFailures(t *testing.T) {
	for _, role := range []domain.TopologyRole{domain.TopologyRoleStandalone, domain.TopologyRoleNode, domain.TopologyRoleStalePrimary} {
		t.Run(string(role), func(t *testing.T) {
			f := newSQLitePublicationFixture(t)
			identity := f.identity
			identity.Role = role
			if _, err := f.read(t, f.store, identity); !errors.Is(err, ErrSnapshotByTokenDenied) {
				t.Fatalf("role=%s error=%v", role, err)
			}
		})
	}
	for _, test := range []struct {
		name string
		edit func(*SnapshotPublicationIdentityRecord)
	}{
		{name: "zero generation", edit: func(i *SnapshotPublicationIdentityRecord) { i.LeadershipGeneration = 0 }},
		{name: "stale generation", edit: func(i *SnapshotPublicationIdentityRecord) { i.LatestKnownGeneration = 8 }},
		{name: "nil cluster key", edit: func(i *SnapshotPublicationIdentityRecord) { i.ClusterKeyID = nil }},
		{name: "wrong cluster key", edit: func(i *SnapshotPublicationIdentityRecord) { wrong := uuid.New(); i.ClusterKeyID = &wrong }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newSQLitePublicationFixture(t)
			identity := f.identity
			test.edit(&identity)
			if _, err := f.read(t, f.store, identity); !errors.Is(err, ErrSnapshotByTokenDenied) {
				t.Fatalf("error=%v want ErrSnapshotByTokenDenied", err)
			}
		})
	}
	t.Run("wrong master key", func(t *testing.T) {
		f := newSQLitePublicationFixture(t)
		wrong := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x38}, 32))
		store := NewSQLiteSnapshotPublicationStore(f.writer, cluster.NewSQLiteKeyStore(f.db, f.writer, wrong))
		if _, err := f.read(t, store, f.identity); !errors.Is(err, ErrSnapshotByTokenUnavailable) {
			t.Fatalf("error=%v want ErrSnapshotByTokenUnavailable", err)
		}
	})
	t.Run("no active key", func(t *testing.T) {
		f := newSQLitePublicationFixture(t)
		settingsExec(t, f.db, `update cluster_keys set retired_at='0'`)
		if _, err := f.read(t, f.store, f.identity); !errors.Is(err, ErrSnapshotByTokenUnavailable) {
			t.Fatalf("error=%v want ErrSnapshotByTokenUnavailable", err)
		}
	})
}

func TestSQLiteSnapshotPublicationByTokenUnavailable(t *testing.T) {
	f := newSQLitePublicationFixture(t)
	f.insertRawBody(t, []byte(`{"a":1}`), true)
	var nilStore *SQLiteSnapshotPublicationStore
	for name, store := range map[string]*SQLiteSnapshotPublicationStore{
		"nil store": nilStore,
		"no writer": NewSQLiteSnapshotPublicationStore(nil, nil),
		"no keys":   NewSQLiteSnapshotPublicationStore(f.writer, nil),
	} {
		if _, err := f.read(t, store, f.identity); !errors.Is(err, ErrSnapshotByTokenUnavailable) {
			t.Fatalf("%s error=%v want ErrSnapshotByTokenUnavailable", name, err)
		}
	}
	if _, err := f.store.ReadSnapshotPublicationByToken(nil, f.selector, f.identity); !errors.Is(err, ErrSnapshotByTokenUnavailable) {
		t.Fatalf("nil context error=%v", err)
	}
}

func TestSQLiteSnapshotPublicationByTokenDatabaseFailure(t *testing.T) {
	f := newSQLitePublicationFixture(t)
	f.db.Close()
	if _, err := f.read(t, f.store, f.identity); !errors.Is(err, ErrSnapshotByTokenUnavailable) {
		t.Fatalf("closed database error=%v want ErrSnapshotByTokenUnavailable", err)
	}
}

func publicationEnvelopeBody(t *testing.T, primaryID string) ([]byte, string) {
	t.Helper()
	env := importTestEnvelope(t)
	env.Transient.SourcePrimaryID = uuid.MustParse(primaryID)
	body, err := replicationsnapshot.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return body, env.ContentHash
}

func publicationRequest(f *sqlitePublicationFixture) SnapshotPublicationRequest {
	return SnapshotPublicationRequest{
		CredentialID: f.credential, PresentedCredential: "secret", MaxBytes: SnapshotPublicationMaxBytes,
		Authenticate: func(presented string, record SnapshotPublicationCredentialRecord) (SnapshotPublicationPrincipal, error) {
			if presented != "secret" || record.CredentialRevokedAt != nil || record.NodeRevokedAt != nil {
				return SnapshotPublicationPrincipal{}, errors.New("bad credential")
			}
			return SnapshotPublicationPrincipal{CredentialID: record.ID, NodeID: record.NodeID}, nil
		},
	}
}

func TestSQLiteSnapshotPublicationReadHappyPath(t *testing.T) {
	f := newSQLitePublicationFixture(t)
	body, hash := publicationEnvelopeBody(t, f.primaryID)
	f.insertSnapshot(t, body, hash, true)
	got, err := f.store.ReadSnapshotPublication(context.Background(), publicationRequest(f))
	if err != nil {
		t.Fatal(err)
	}
	if got.Current || !bytes.Equal(got.Snapshot.Bytes, body) || got.Snapshot.ContentHash != hash || !got.Snapshot.ProofComplete ||
		got.Snapshot.RevisionNumber != 1 || got.Snapshot.SourcePrimaryID != f.primaryID || got.Snapshot.LeadershipGeneration != 7 ||
		got.Snapshot.RevisionID == "" || got.Snapshot.ApplyJobID == "" || got.Snapshot.AppliedAt.IsZero() || got.Snapshot.DiscardedAt != nil {
		t.Fatalf("snapshot=%+v", got.Snapshot)
	}
	if got.Principal.CredentialID != f.credential || got.Principal.NodeID != f.peerID ||
		got.Identity.InstallationID != f.primaryID || !got.Identity.ClusterKeyUsable || got.Identity.Role != domain.TopologyRolePrimary {
		t.Fatalf("principal=%+v identity=%+v", got.Principal, got.Identity)
	}
}

func TestSQLiteSnapshotPublicationReadCurrentOmitsBytes(t *testing.T) {
	f := newSQLitePublicationFixture(t)
	body, hash := publicationEnvelopeBody(t, f.primaryID)
	f.insertSnapshot(t, body, hash, true)
	request := publicationRequest(f)
	request.AfterContentHash = hash
	got, err := f.store.ReadSnapshotPublication(context.Background(), request)
	if err != nil || !got.Current || got.Snapshot.Bytes != nil || got.Snapshot.ContentHash != hash {
		t.Fatalf("result=%+v err=%v", got, err)
	}
}

func TestSQLiteSnapshotPublicationReadDenials(t *testing.T) {
	valid := func(t *testing.T) *sqlitePublicationFixture {
		f := newSQLitePublicationFixture(t)
		body, hash := publicationEnvelopeBody(t, f.primaryID)
		f.insertSnapshot(t, body, hash, true)
		return f
	}
	for _, test := range []struct {
		name  string
		setup func(*testing.T, *sqlitePublicationFixture, *SnapshotPublicationRequest)
	}{
		{"empty credential id", func(_ *testing.T, _ *sqlitePublicationFixture, r *SnapshotPublicationRequest) { r.CredentialID = "" }},
		{"empty presented credential", func(_ *testing.T, _ *sqlitePublicationFixture, r *SnapshotPublicationRequest) {
			r.PresentedCredential = ""
		}},
		{"nil authenticator", func(_ *testing.T, _ *sqlitePublicationFixture, r *SnapshotPublicationRequest) { r.Authenticate = nil }},
		{"zero max bytes", func(_ *testing.T, _ *sqlitePublicationFixture, r *SnapshotPublicationRequest) { r.MaxBytes = 0 }},
		{"max bytes above limit", func(_ *testing.T, _ *sqlitePublicationFixture, r *SnapshotPublicationRequest) {
			r.MaxBytes = SnapshotPublicationMaxBytes + 1
		}},
		{"unknown credential", func(_ *testing.T, _ *sqlitePublicationFixture, r *SnapshotPublicationRequest) {
			r.CredentialID = uuid.NewString()
		}},
		{"authentication failure", func(_ *testing.T, _ *sqlitePublicationFixture, r *SnapshotPublicationRequest) {
			r.PresentedCredential = "other"
		}},
		{"principal mismatch", func(_ *testing.T, _ *sqlitePublicationFixture, r *SnapshotPublicationRequest) {
			r.Authenticate = func(string, SnapshotPublicationCredentialRecord) (SnapshotPublicationPrincipal, error) {
				return SnapshotPublicationPrincipal{CredentialID: "x", NodeID: "y"}, nil
			}
		}},
		{"revoked credential", func(t *testing.T, f *sqlitePublicationFixture, _ *SnapshotPublicationRequest) {
			settingsExec(t, f.db, `update node_credentials set revoked_at=?`, tokenKey(t, time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)))
		}},
		{"revoked node", func(t *testing.T, f *sqlitePublicationFixture, _ *SnapshotPublicationRequest) {
			settingsExec(t, f.db, `update enrolled_nodes set revoked_at=?`, tokenKey(t, time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)))
		}},
		{"invalid after hash", func(_ *testing.T, _ *sqlitePublicationFixture, r *SnapshotPublicationRequest) {
			r.AfterContentHash = strings.Repeat("A", 64)
		}},
		{"node role", func(t *testing.T, f *sqlitePublicationFixture, _ *SnapshotPublicationRequest) {
			settingsExec(t, f.db, `update installation_identity set role='node'`)
		}},
		{"stale generation", func(t *testing.T, f *sqlitePublicationFixture, _ *SnapshotPublicationRequest) {
			settingsExec(t, f.db, `update installation_identity set latest_known_generation=8`)
		}},
		{"primary mismatch", func(t *testing.T, f *sqlitePublicationFixture, _ *SnapshotPublicationRequest) {
			settingsExec(t, f.db, `update installation_identity set installation_id=?`, uuid.NewString())
		}},
		{"missing cluster key binding", func(t *testing.T, f *sqlitePublicationFixture, _ *SnapshotPublicationRequest) {
			settingsExec(t, f.db, `update installation_identity set cluster_key_id=null`)
		}},
		{"size above max bytes", func(_ *testing.T, _ *sqlitePublicationFixture, r *SnapshotPublicationRequest) { r.MaxBytes = 10 }},
		{"corrupt body", func(t *testing.T, f *sqlitePublicationFixture, _ *SnapshotPublicationRequest) {
			settingsExec(t, f.db, `update applied_snapshots set snapshot_body=cast('not json' as blob)`)
		}},
		{"incomplete newest candidate has no fallback", func(t *testing.T, f *sqlitePublicationFixture, _ *SnapshotPublicationRequest) {
			f.insertRawBody(t, []byte(`{"newer":true}`), false)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := valid(t)
			request := publicationRequest(f)
			test.setup(t, f, &request)
			if _, err := f.store.ReadSnapshotPublication(context.Background(), request); !errors.Is(err, ErrSnapshotPublicationStore) {
				t.Fatalf("error=%v want ErrSnapshotPublicationStore", err)
			}
		})
	}
}

func TestSQLiteSnapshotPublicationReadRecordProjection(t *testing.T) {
	f := newSQLitePublicationFixture(t)
	settingsExec(t, f.db, `update enrolled_nodes set revoked_at=?`, tokenKey(t, time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)))
	var seen SnapshotPublicationCredentialRecord
	request := publicationRequest(f)
	request.Authenticate = func(_ string, r SnapshotPublicationCredentialRecord) (SnapshotPublicationPrincipal, error) {
		seen = r
		return SnapshotPublicationPrincipal{CredentialID: r.ID, NodeID: r.NodeID}, nil
	}
	_, _ = f.store.ReadSnapshotPublication(context.Background(), request)
	if seen.ID != f.credential || seen.NodeID != f.peerID || seen.PrimaryID != f.primaryID || seen.HashVersion != "fixture" ||
		seen.CredentialHash != "synthetic" || seen.CredentialRevokedAt != nil || seen.NodeRevokedAt == nil {
		t.Fatalf("record=%+v", seen)
	}
}

func TestSQLiteSnapshotPublicationReadUnavailable(t *testing.T) {
	f := newSQLitePublicationFixture(t)
	var nilStore *SQLiteSnapshotPublicationStore
	for name, store := range map[string]*SQLiteSnapshotPublicationStore{
		"nil store": nilStore,
		"no writer": NewSQLiteSnapshotPublicationStore(nil, nil),
		"no keys":   NewSQLiteSnapshotPublicationStore(f.writer, nil),
	} {
		if _, err := store.ReadSnapshotPublication(context.Background(), publicationRequest(f)); !errors.Is(err, ErrSnapshotPublicationStore) {
			t.Fatalf("%s error=%v", name, err)
		}
	}
	if _, err := f.store.ReadSnapshotPublication(nil, publicationRequest(f)); !errors.Is(err, ErrSnapshotPublicationStore) {
		t.Fatalf("nil context error=%v", err)
	}
	f.db.Close()
	if _, err := f.store.ReadSnapshotPublication(context.Background(), publicationRequest(f)); !errors.Is(err, ErrSnapshotPublicationStore) {
		t.Fatalf("closed database error=%v", err)
	}
}

func TestSQLiteLatestPublishedContentHash(t *testing.T) {
	f := newSQLitePublicationFixture(t)
	if hash, ok, err := f.store.LatestPublishedContentHash(context.Background()); err != nil || ok || hash != "" {
		t.Fatalf("empty store: hash=%q ok=%v err=%v", hash, ok, err)
	}
	body, hash := publicationEnvelopeBody(t, f.primaryID)
	f.insertSnapshot(t, body, hash, true)
	got, ok, err := f.store.LatestPublishedContentHash(context.Background())
	if err != nil || !ok || got != hash {
		t.Fatalf("hash=%q ok=%v err=%v, want %q", got, ok, err, hash)
	}
	// A newer candidate whose apply is not terminal is not publishable, and the
	// publication never falls back to an older one (matches ReadSnapshotPublication).
	f.insertRawBody(t, []byte(`{"pending":true}`), false)
	if got, ok, err := f.store.LatestPublishedContentHash(context.Background()); err != nil || ok {
		t.Fatalf("incomplete newest: hash=%q ok=%v err=%v", got, ok, err)
	}
}
