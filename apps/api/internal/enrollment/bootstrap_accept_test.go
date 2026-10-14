package enrollment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/cluster"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

var acceptTestMaster = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x19}, 32))

type acceptFixture struct {
	db        *sql.DB
	writer    *sqlitedb.Writer
	keys      *cluster.SQLiteKeyStore
	acceptor  *BootstrapAcceptor
	recipient BootstrapRecipient
	binding   BootstrapBinding
	credID    string
	keyID     string
	now       time.Time
}

func newAcceptFixture(t *testing.T) *acceptFixture {
	t.Helper()
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "accept.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := sqlitedb.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	f := &acceptFixture{db: db, now: time.Unix(1_800_000_000, 0).UTC(), credID: uuid.NewString(), keyID: uuid.NewString()}
	f.writer = sqlitedb.NewWriter(db, nil)
	f.keys = cluster.NewSQLiteKeyStore(db, f.writer, acceptTestMaster)
	f.acceptor = NewBootstrapAcceptor(BootstrapAcceptorOptions{
		DB: db, Writer: f.writer, Keys: f.keys, MasterKeyBase64: acceptTestMaster, Now: func() time.Time { return f.now },
	})
	f.recipient, err = NewBootstrapRecipient()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.recipient.Destroy)
	f.binding = testBootstrapBinding()
	return f
}

func (f *acceptFixture) seal(t *testing.T, binding BootstrapBinding, expiresAt time.Time) ([]byte, string) {
	t.Helper()
	credential, kek := testBootstrapInputs()
	grant, err := NewBootstrapGrant(BootstrapGrantMetadata{
		ProtocolVersion:             binding.ProtocolVersion,
		CredentialID:                f.credID,
		ClusterKeyID:                f.keyID,
		SourcePrimaryInstallationID: binding.SourcePrimaryInstallationID,
		TargetInstallationID:        binding.TargetInstallationID,
		TargetNodeID:                binding.TargetNodeID,
		AttemptID:                   binding.AttemptID,
		LeadershipGeneration:        binding.LeadershipGeneration,
		ExpiresAtUnix:               expiresAt.Unix(),
	}, credential, kek)
	if err != nil {
		t.Fatal(err)
	}
	defer grant.Destroy()
	public, err := f.recipient.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := SealBootstrap(public, binding, grant)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(sealed)
	return sealed, hex.EncodeToString(sum[:])
}

func (f *acceptFixture) count(t *testing.T, query string) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *acceptFixture) assertNothingPersisted(t *testing.T) {
	t.Helper()
	if got := f.count(t, `select count(*) from cluster_keys`); got != 0 {
		t.Fatalf("cluster_keys rows = %d, want 0", got)
	}
	if got := f.count(t, `select count(*) from secrets where purpose = 'node-credential'`); got != 0 {
		t.Fatalf("node credential secrets = %d, want 0", got)
	}
}

func TestBootstrapAcceptorPersistsKEKAndCredentialAtomically(t *testing.T) {
	f := newAcceptFixture(t)
	sealed, hash := f.seal(t, f.binding, f.now.Add(time.Hour))
	accepted, err := f.acceptor.Accept(context.Background(), &f.recipient, f.binding, sealed, hash)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if accepted.CredentialID != f.credID || accepted.ClusterKeyID != f.keyID ||
		accepted.SourcePrimaryInstallationID != f.binding.SourcePrimaryInstallationID ||
		accepted.LeadershipGeneration != f.binding.LeadershipGeneration || !accepted.ExpiresAt.Equal(f.now.Add(time.Hour)) {
		t.Fatalf("accepted = %+v", accepted)
	}
	_, kek := testBootstrapInputs()
	keyUUID := uuid.MustParse(f.keyID)
	material, err := f.keys.LoadActive(context.Background(), keyUUID)
	if err != nil {
		t.Fatalf("LoadActive: %v", err)
	}
	defer material.Destroy()
	if !bytes.Equal(material.CopyBytes(), kek) {
		t.Fatal("persisted KEK mismatch")
	}
	secret, err := f.acceptor.LoadNodeCredentialSecret(context.Background(), f.credID)
	if err != nil {
		t.Fatalf("LoadNodeCredentialSecret: %v", err)
	}
	credential, _ := testBootstrapInputs()
	if !bytes.Equal(secret, credential) {
		t.Fatal("persisted credential mismatch")
	}
	zeroBytes(secret)
}

func TestBootstrapAcceptorWrongBindingPersistsNothing(t *testing.T) {
	f := newAcceptFixture(t)
	sealed, hash := f.seal(t, f.binding, f.now.Add(time.Hour))
	wrong := f.binding
	wrong.TargetNodeID = "another-node"
	if _, err := f.acceptor.Accept(context.Background(), &f.recipient, wrong, sealed, hash); !errors.Is(err, ErrBootstrapDenied) {
		t.Fatalf("err = %v, want ErrBootstrapDenied", err)
	}
	f.assertNothingPersisted(t)
}

func TestBootstrapAcceptorPayloadHashMismatchPersistsNothing(t *testing.T) {
	f := newAcceptFixture(t)
	sealed, _ := f.seal(t, f.binding, f.now.Add(time.Hour))
	if _, err := f.acceptor.Accept(context.Background(), &f.recipient, f.binding, sealed, hex.EncodeToString(make([]byte, 32))); !errors.Is(err, ErrBootstrapDenied) {
		t.Fatalf("err = %v, want ErrBootstrapDenied", err)
	}
	f.assertNothingPersisted(t)
}

func TestBootstrapAcceptorExpiredPayloadPersistsNothing(t *testing.T) {
	f := newAcceptFixture(t)
	sealed, hash := f.seal(t, f.binding, f.now.Add(-time.Second))
	if _, err := f.acceptor.Accept(context.Background(), &f.recipient, f.binding, sealed, hash); !errors.Is(err, ErrBootstrapDenied) {
		t.Fatalf("err = %v, want ErrBootstrapDenied", err)
	}
	f.assertNothingPersisted(t)
}

func TestBootstrapAcceptorRollsBackWhenFailureFallsBetweenWrites(t *testing.T) {
	f := newAcceptFixture(t)
	sealed, hash := f.seal(t, f.binding, f.now.Add(time.Hour))
	f.acceptor.afterKEKWrite = func() error { return errors.New("injected failure") }
	if _, err := f.acceptor.Accept(context.Background(), &f.recipient, f.binding, sealed, hash); err == nil {
		t.Fatal("Accept succeeded despite injected failure")
	}
	f.assertNothingPersisted(t)
}

func TestBootstrapAcceptorZeroesPlaintextOnSuccessAndFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		f := newAcceptFixture(t)
		sealed, hash := f.seal(t, f.binding, f.now.Add(time.Hour))
		var observed []*BootstrapGrant
		f.acceptor.onGrant = func(g *BootstrapGrant) { observed = append(observed, g) }
		var credView, kekView []byte
		f.acceptor.observeSecrets = func(credential, kek []byte) { credView, kekView = credential, kek }
		if fail {
			f.acceptor.afterKEKWrite = func() error { return errors.New("injected failure") }
		}
		_, err := f.acceptor.Accept(context.Background(), &f.recipient, f.binding, sealed, hash)
		if (err != nil) != fail {
			t.Fatalf("fail=%v err=%v", fail, err)
		}
		if len(observed) != 1 || len(credView) != bootstrapCredentialBytes || len(kekView) != bootstrapKEKBytes {
			t.Fatalf("fail=%v observed grants=%d", fail, len(observed))
		}
		if !allZero(credView) || !allZero(kekView) {
			t.Fatalf("fail=%v plaintext buffers not zeroed", fail)
		}
		if observed[0].Secrets.NodeCredential.bytes != nil || observed[0].Secrets.ClusterKEK.bytes != nil {
			t.Fatalf("fail=%v grant not destroyed", fail)
		}
	}
}

func TestBootstrapAcceptorReplayIsIdempotent(t *testing.T) {
	f := newAcceptFixture(t)
	sealed, hash := f.seal(t, f.binding, f.now.Add(time.Hour))
	for i := 0; i < 2; i++ {
		if _, err := f.acceptor.Accept(context.Background(), &f.recipient, f.binding, sealed, hash); err != nil {
			t.Fatalf("Accept #%d: %v", i+1, err)
		}
	}
	if got := f.count(t, `select count(*) from cluster_keys`); got != 1 {
		t.Fatalf("cluster_keys rows = %d, want 1", got)
	}
	if got := f.count(t, `select count(*) from secrets where purpose = 'node-credential'`); got != 1 {
		t.Fatalf("credential secrets = %d, want 1", got)
	}
}

func TestBootstrapAcceptorConflictingReplayDenied(t *testing.T) {
	f := newAcceptFixture(t)
	sealed, hash := f.seal(t, f.binding, f.now.Add(time.Hour))
	if _, err := f.acceptor.Accept(context.Background(), &f.recipient, f.binding, sealed, hash); err != nil {
		t.Fatal(err)
	}
	// Same ids, different secret material: must not overwrite the stored KEK.
	other, err := NewBootstrapGrant(BootstrapGrantMetadata{
		ProtocolVersion: f.binding.ProtocolVersion, CredentialID: f.credID, ClusterKeyID: f.keyID,
		SourcePrimaryInstallationID: f.binding.SourcePrimaryInstallationID, TargetInstallationID: f.binding.TargetInstallationID,
		TargetNodeID: f.binding.TargetNodeID, AttemptID: f.binding.AttemptID,
		LeadershipGeneration: f.binding.LeadershipGeneration, ExpiresAtUnix: f.now.Add(time.Hour).Unix(),
	}, bytes.Repeat([]byte{0x33}, 32), bytes.Repeat([]byte{0x44}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Destroy()
	public, _ := f.recipient.PublicKey()
	sealed2, err := SealBootstrap(public, f.binding, other)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(sealed2)
	if _, err := f.acceptor.Accept(context.Background(), &f.recipient, f.binding, sealed2, hex.EncodeToString(sum[:])); !errors.Is(err, ErrBootstrapDenied) {
		t.Fatalf("err = %v, want ErrBootstrapDenied", err)
	}
	_, kek := testBootstrapInputs()
	material, err := f.keys.LoadActive(context.Background(), uuid.MustParse(f.keyID))
	if err != nil {
		t.Fatal(err)
	}
	defer material.Destroy()
	if !bytes.Equal(material.CopyBytes(), kek) {
		t.Fatal("stored KEK was overwritten")
	}
}

func allZero(value []byte) bool {
	for _, b := range value {
		if b != 0 {
			return false
		}
	}
	return true
}
