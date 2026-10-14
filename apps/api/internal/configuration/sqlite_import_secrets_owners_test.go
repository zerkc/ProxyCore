package configuration

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/cluster"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
)

const (
	importedSecretPlaintext = "primary-certificate-private-key-PEM"
	importedOwnerHash       = "scrypt$primary-owner-hash"
)

// clusterKEK reads the fixture's active cluster key as a wrapping KEK, the key
// the primary used to wrap the replicated secrets.
func clusterKEK(t *testing.T, f *sqlitePublicationFixture) *cluster.KEK {
	t.Helper()
	keys := cluster.NewSQLiteKeyStore(f.db, f.writer, f.master)
	material, err := keys.LoadActive(context.Background(), f.clusterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer material.Destroy()
	raw := material.CopyBytes()
	defer func() {
		for i := range raw {
			raw[i] = 0
		}
	}()
	kek, err := cluster.NewKEK(raw)
	if err != nil {
		t.Fatal(err)
	}
	return kek
}

func wrappedSecret(t *testing.T, kek *cluster.KEK, id uuid.UUID, purpose, plaintext string) replicationsnapshot.ReplicatedSecret {
	t.Helper()
	sealed, err := kek.Wrap([]byte(plaintext))
	if err != nil {
		t.Fatal(err)
	}
	return replicationsnapshot.ReplicatedSecret{ID: id, Purpose: purpose, Envelope: sealed}
}

func importWith(t *testing.T, f *sqlitePublicationFixture, store *SQLitePhase2Store, mutate func(*replicationsnapshot.Envelope)) error {
	t.Helper()
	cfg := materializeConfig()
	body, _ := materializeEnvelopeBody(t, f.primaryID, cfg, mutate)
	_, err := store.CreateApplyJobFromSnapshot(context.Background(), "import", body)
	return err
}

func TestSQLiteImportMaterializesSecretsRewrappedWithLocalMasterKey(t *testing.T) {
	f, store := newSQLitePhase2Fixture(t)
	kek := clusterKEK(t, f)
	secretID := uuid.MustParse("44444444-aaaa-4aaa-8aaa-444444444444")
	keyRef := f.clusterKey
	err := importWith(t, f, store, func(env *replicationsnapshot.Envelope) {
		env.NodeLocal.ClusterKeyRef = &keyRef
		env.Replicated.Secrets = []replicationsnapshot.ReplicatedSecret{wrappedSecret(t, kek, secretID, "certificate-key", importedSecretPlaintext)}
		env.Replicated.Configuration = withCertificateSecret(materializeConfig(), secretID.String())
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := secrets.NewSQLiteStore(f.db, f.master).Get(context.Background(), secretID.String())
	if err != nil || got != importedSecretPlaintext {
		t.Fatalf("local secret store must read the imported plaintext, got %q, %v", got, err)
	}
	var ciphertext, purpose string
	if err := f.db.QueryRow(`select ciphertext, purpose from secrets where id = ?`, secretID.String()).Scan(&ciphertext, &purpose); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ciphertext, importedSecretPlaintext) || strings.HasPrefix(ciphertext, "v1.kek") || !strings.HasPrefix(ciphertext, "v1:") || purpose != "certificate-key" {
		t.Fatalf("secret must be stored locally wrapped (purpose=%q): %q", purpose, ciphertext)
	}
	if n := phase2Count(t, f, `select count(*) from certificates where key_secret_id = ?`, secretID.String()); n != 1 {
		t.Fatalf("certificate key_secret_id must resolve to the imported secret, got %d", n)
	}
}

func TestSQLiteImportFailsClosedOnWrongOrMissingKEK(t *testing.T) {
	cases := map[string]func(t *testing.T, f *sqlitePublicationFixture, env *replicationsnapshot.Envelope){
		"wrong kek": func(t *testing.T, f *sqlitePublicationFixture, env *replicationsnapshot.Envelope) {
			raw, err := cluster.GenerateKEK()
			if err != nil {
				t.Fatal(err)
			}
			other, err := cluster.NewKEK(raw)
			if err != nil {
				t.Fatal(err)
			}
			env.Replicated.Secrets = []replicationsnapshot.ReplicatedSecret{wrappedSecret(t, other, uuid.New(), "certificate-key", "x")}
		},
		"unknown key ref": func(t *testing.T, f *sqlitePublicationFixture, env *replicationsnapshot.Envelope) {
			unknown := uuid.New()
			env.NodeLocal.ClusterKeyRef = &unknown
			env.Replicated.Secrets = []replicationsnapshot.ReplicatedSecret{wrappedSecret(t, clusterKEK(t, f), uuid.New(), "certificate-key", "x")}
		},
		"no key ref": func(t *testing.T, f *sqlitePublicationFixture, env *replicationsnapshot.Envelope) {
			env.NodeLocal.ClusterKeyRef = nil
			env.Replicated.Secrets = []replicationsnapshot.ReplicatedSecret{wrappedSecret(t, clusterKEK(t, f), uuid.New(), "certificate-key", "x")}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f, store := newSQLitePhase2Fixture(t)
			keyRef := f.clusterKey
			secretsBefore := phase2Count(t, f, `select count(*) from secrets`)
			usersBefore := phase2Count(t, f, `select count(*) from users`)
			err := importWith(t, f, store, func(env *replicationsnapshot.Envelope) {
				env.NodeLocal.ClusterKeyRef = &keyRef
				env.Replicated.Owners = []replicationsnapshot.ReplicatedOwner{{UserID: uuid.New(), Username: "primary-owner", PasswordHash: importedOwnerHash, Role: "owner"}}
				mutate(t, f, env)
			})
			if err == nil {
				t.Fatal("import must fail closed")
			}
			for query, want := range map[string]int{
				`select count(*) from config_revisions where source = 'import'`: 0,
				`select count(*) from zones`:                                    0,
				`select count(*) from secrets`:                                  secretsBefore,
				`select count(*) from users`:                                    usersBefore,
			} {
				if n := phase2Count(t, f, query); n != want {
					t.Fatalf("%s = %d, want %d (rollback)", query, n, want)
				}
			}
		})
	}
}

func TestSQLiteImportMaterializesOwners(t *testing.T) {
	f, store := newSQLitePhase2Fixture(t)
	ownerID := uuid.New()
	if err := importWith(t, f, store, func(env *replicationsnapshot.Envelope) {
		env.Replicated.Owners = []replicationsnapshot.ReplicatedOwner{
			{UserID: ownerID, Username: "primary-owner", PasswordHash: importedOwnerHash, Role: "owner"},
		}
	}); err != nil {
		t.Fatal(err)
	}
	var username, hash, role string
	var active int
	if err := f.db.QueryRow(`select username, password_hash, role, active from users where id = ?`, ownerID.String()).Scan(&username, &hash, &role, &active); err != nil {
		t.Fatal(err)
	}
	if username != "primary-owner" || hash != importedOwnerHash || role != "owner" || active != 1 {
		t.Fatalf("owner not imported: %q %q %q %d", username, hash, role, active)
	}
	// The pre-existing local owner is referenced by history (enrollment token)
	// so it is deactivated, never deleted, and cannot log in any more.
	if n := phase2Count(t, f, `select count(*) from users where username = 'synthetic-owner' and active = 0`); n != 1 {
		t.Fatalf("local-only owner must be deactivated, got %d", n)
	}
}

func TestSQLiteImportOwnerWithExistingUsernameKeepsLocalIDAndTakesPrimaryCredentials(t *testing.T) {
	f, store := newSQLitePhase2Fixture(t)
	var localID string
	if err := f.db.QueryRow(`select id from users where username = 'synthetic-owner'`).Scan(&localID); err != nil {
		t.Fatal(err)
	}
	if err := importWith(t, f, store, func(env *replicationsnapshot.Envelope) {
		env.Replicated.Owners = []replicationsnapshot.ReplicatedOwner{
			{UserID: uuid.New(), Username: "synthetic-owner", PasswordHash: importedOwnerHash, Role: "owner"},
		}
	}); err != nil {
		t.Fatal(err)
	}
	var hash string
	var active int
	if err := f.db.QueryRow(`select password_hash, active from users where id = ?`, localID).Scan(&hash, &active); err != nil {
		t.Fatal(err)
	}
	if hash != importedOwnerHash || active != 1 || phase2Count(t, f, `select count(*) from users`) != 1 {
		t.Fatalf("same-username owner must be updated in place: %q %d", hash, active)
	}
}

func TestSQLiteImportWithoutOwnersNeverLocksTheNodeOut(t *testing.T) {
	f, store := newSQLitePhase2Fixture(t)
	if err := importWith(t, f, store, nil); err != nil {
		t.Fatal(err)
	}
	if n := phase2Count(t, f, `select count(*) from users where role = 'owner' and active = 1`); n != 1 {
		t.Fatalf("an envelope without owners must leave local owners untouched, active owners = %d", n)
	}
}

func TestSQLiteReimportReplacesSecretsAndOwnersWithoutMerging(t *testing.T) {
	f, store := newSQLitePhase2Fixture(t)
	kek := clusterKEK(t, f)
	keyRef := f.clusterKey
	idA, idB := uuid.New(), uuid.New()
	ownerA, ownerB := uuid.New(), uuid.New()
	first := func(env *replicationsnapshot.Envelope) {
		env.NodeLocal.ClusterKeyRef = &keyRef
		env.Replicated.Secrets = []replicationsnapshot.ReplicatedSecret{wrappedSecret(t, kek, idA, "dns", "secret-a")}
		env.Replicated.Owners = []replicationsnapshot.ReplicatedOwner{{UserID: ownerA, Username: "owner-a", PasswordHash: "h-a", Role: "owner"}}
	}
	if err := importWith(t, f, store, first); err != nil {
		t.Fatal(err)
	}
	if err := importWith(t, f, store, func(env *replicationsnapshot.Envelope) {
		env.Transient.LeadershipGeneration++
		env.NodeLocal.ClusterKeyRef = &keyRef
		env.Replicated.Secrets = []replicationsnapshot.ReplicatedSecret{
			wrappedSecret(t, kek, idA, "dns", "secret-a-rotated"),
			wrappedSecret(t, kek, idB, "dns", "secret-b"),
		}
		env.Replicated.Owners = []replicationsnapshot.ReplicatedOwner{
			{UserID: ownerA, Username: "owner-a", PasswordHash: "h-a2", Role: "owner"},
			{UserID: ownerB, Username: "owner-b", PasswordHash: "h-b", Role: "operator"},
		}
	}); err != nil {
		t.Fatal(err)
	}
	store2 := secrets.NewSQLiteStore(f.db, f.master)
	for id, want := range map[uuid.UUID]string{idA: "secret-a-rotated", idB: "secret-b"} {
		if got, err := store2.Get(context.Background(), id.String()); err != nil || got != want {
			t.Fatalf("secret %s = %q, %v; want %q", id, got, err, want)
		}
	}
	var hash string
	if err := f.db.QueryRow(`select password_hash from users where id = ?`, ownerA.String()).Scan(&hash); err != nil || hash != "h-a2" {
		t.Fatalf("owner-a hash = %q, %v", hash, err)
	}
	// A third import that drops owner-b deactivates it (replace, not merge).
	if err := importWith(t, f, store, func(env *replicationsnapshot.Envelope) {
		env.Transient.LeadershipGeneration += 2
		env.Replicated.Owners = []replicationsnapshot.ReplicatedOwner{{UserID: ownerA, Username: "owner-a", PasswordHash: "h-a2", Role: "owner"}}
	}); err != nil {
		t.Fatal(err)
	}
	if n := phase2Count(t, f, `select count(*) from users where id = ? and active = 0`, ownerB.String()); n != 1 {
		t.Fatalf("owner dropped by the primary must be deactivated, got %d", n)
	}
}

func TestSQLiteImportPreservesNodeLocalSecrets(t *testing.T) {
	f, store := newSQLitePhase2Fixture(t)
	local, err := secrets.NewSQLiteStore(f.db, f.master).Put(context.Background(), "node-credential", "node-local-credential")
	if err != nil {
		t.Fatal(err)
	}
	if err := importWith(t, f, store, nil); err != nil {
		t.Fatal(err)
	}
	got, err := secrets.NewSQLiteStore(f.db, f.master).Get(context.Background(), local)
	if err != nil || got != "node-local-credential" {
		t.Fatalf("node-local secret must survive an import, got %q, %v", got, err)
	}
}

func withCertificateSecret(cfg domain.ConfigurationSnapshot, secretID string) domain.ConfigurationSnapshot {
	cfg.Certificates[0].SecretID = secretID
	return cfg
}

func listOwnersOf(t *testing.T, f *sqlitePublicationFixture, store *SQLitePhase2Store) []replicationsnapshot.ReplicableOwner {
	t.Helper()
	var owners []replicationsnapshot.ReplicableOwner
	err := store.WithCanonicalSnapshot(context.Background(), func(tx CanonicalSnapshotTransaction) error {
		var err error
		owners, err = tx.ListReplicableOwners(context.Background())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return owners
}

func TestSQLiteExportOmitsInactiveUsersAndNodeKeepsThemInactive(t *testing.T) {
	f, store := newSQLitePhase2Fixture(t)
	settingsExec(t, f.db, `insert into users(id,username,password_hash,role,active,created_at,updated_at) values (?,'disabled-op','scrypt$x','operator',0,'0','0')`, uuid.NewString())
	owners := listOwnersOf(t, f, store)
	if len(owners) != 1 || owners[0].Username != "synthetic-owner" || owners[0].Role != "owner" {
		t.Fatalf("exported owners = %+v, want only the active owner", owners)
	}
	// An inactive owner is not exported either; only the active owner remains.
	settingsExec(t, f.db, `insert into users(id,username,password_hash,role,active,created_at,updated_at) values (?,'disabled-owner','scrypt$y','owner',0,'0','0')`, uuid.NewString())
	if got := listOwnersOf(t, f, store); len(got) != 1 || got[0].Username != "synthetic-owner" {
		t.Fatalf("inactive owner exported: %+v", got)
	}
	// A node importing the exported set leaves non-exported local users inactive.
	exported := make([]replicationsnapshot.ReplicatedOwner, 0, len(owners))
	for _, o := range owners {
		exported = append(exported, replicationsnapshot.ReplicatedOwner{UserID: o.UserID, Username: o.Username, PasswordHash: "scrypt$synthetic", Role: o.Role})
	}
	node, nodeStore := newSQLitePhase2Fixture(t)
	settingsExec(t, node.db, `insert into users(id,username,password_hash,role,created_at,updated_at) values (?,'disabled-op','scrypt$x','operator','0','0')`, uuid.NewString())
	if err := importWith(t, node, nodeStore, func(env *replicationsnapshot.Envelope) { env.Replicated.Owners = exported }); err != nil {
		t.Fatal(err)
	}
	if n := phase2Count(t, node, `select count(*) from users where username = 'disabled-op' and active = 0`); n != 1 {
		t.Fatalf("user absent from the snapshot must be inactive on the node, got %d", n)
	}
}

func TestSQLiteImportActiveOwnersOnlyOperatorsFailsClosed(t *testing.T) {
	f, store := newSQLitePhase2Fixture(t)
	err := importWith(t, f, store, func(env *replicationsnapshot.Envelope) {
		env.Replicated.Owners = []replicationsnapshot.ReplicatedOwner{
			{UserID: uuid.New(), Username: "only-operator", PasswordHash: importedOwnerHash, Role: "operator"},
		}
	})
	if err == nil {
		t.Fatal("snapshot with owners but no role=owner must fail closed")
	}
}

func TestSQLiteImportV3VerifiesSecretDigestAgainstUnwrappedPlaintext(t *testing.T) {
	secretID := uuid.MustParse("55555555-aaaa-4aaa-8aaa-555555555555")
	for _, tc := range []struct {
		name      string
		plaintext string // the value the digest is computed over
		wantErr   bool
	}{
		{"matching digest imports", importedSecretPlaintext, false},
		{"digest of another value is rejected", "some-other-value", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, store := newSQLitePhase2Fixture(t)
			kek := clusterKEK(t, f)
			keyRef := f.clusterKey
			digest, err := kek.SecretDigest(secretID.String(), "certificate-key", []byte(tc.plaintext))
			if err != nil {
				t.Fatal(err)
			}
			err = importWith(t, f, store, func(env *replicationsnapshot.Envelope) {
				env.NodeLocal.ClusterKeyRef = &keyRef
				secret := wrappedSecret(t, kek, secretID, "certificate-key", importedSecretPlaintext)
				secret.Digest = digest
				env.HashVersion = replicationsnapshot.HashVersionStableSecrets
				env.Replicated.Secrets = []replicationsnapshot.ReplicatedSecret{secret}
				env.Replicated.Configuration = withCertificateSecret(materializeConfig(), secretID.String())
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				if n := phase2Count(t, f, `select count(*) from secrets where id = ?`, secretID.String()); n != 0 {
					t.Fatal("rejected import must roll back")
				}
			}
		})
	}
}
