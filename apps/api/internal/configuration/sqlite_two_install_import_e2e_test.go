package configuration

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/cluster"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// The two installations below model what the install script produces: each
// instance generates its own master key and its own cluster key material, and
// the only thing they share is the cluster KEK the node receives at enrollment.

// randomMasterKey generates a master key the way an install does: 32 random
// bytes, base64 encoded, never derived from a constant.
func randomMasterKey(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

type fixedExportConfig struct{ snapshot domain.ConfigurationSnapshot }

func (c fixedExportConfig) Snapshot(context.Context) (domain.ConfigurationSnapshot, error) {
	return c.snapshot, nil
}

type twoInstalls struct {
	primary, node           *sqlitePublicationFixture
	primaryStore, nodeStore *SQLitePhase2Store
	primarySecrets          map[uuid.UUID]string // id -> plaintext
	certSecretID            uuid.UUID
}

func newTwoInstalls(t *testing.T) *twoInstalls {
	t.Helper()
	ti := &twoInstalls{primarySecrets: map[uuid.UUID]string{}}
	ti.primary = newSQLitePublicationFixtureWithMaster(t, randomMasterKey(t))
	ti.node = newSQLitePublicationFixtureWithMaster(t, randomMasterKey(t))
	if ti.primary.master == ti.node.master {
		t.Fatal("installs must not share a master key")
	}
	if ti.primary.clusterKey == ti.node.clusterKey {
		t.Fatal("installs must not share a generated cluster key")
	}
	ti.primaryStore = NewSQLitePhase2Store(ti.primary.db, ti.primary.writer,
		cluster.NewSQLiteKeyStore(ti.primary.db, ti.primary.writer, ti.primary.master), ti.primary.master)
	ti.nodeStore = NewSQLitePhase2Store(ti.node.db, ti.node.writer,
		cluster.NewSQLiteKeyStore(ti.node.db, ti.node.writer, ti.node.master), ti.node.master)

	// The primary's owner carries a real scrypt hash, as a bootstrapped install does.
	ownerHash, err := auth.HashPassword("synthetic-owner-password")
	if err != nil {
		t.Fatal(err)
	}
	settingsExec(t, ti.primary.db, `update users set password_hash = ?`, ownerHash)

	// Primary secrets are written with the production purposes through the real
	// secret store, encrypted with the primary's own master key.
	store := secrets.NewSQLiteStore(ti.primary.db, ti.primary.master)
	hash, err := secrets.HashBasicAuthPassword("synthetic-basic-auth-password")
	if err != nil {
		t.Fatal(err)
	}
	for purpose, plaintext := range map[string]string{
		"basic-auth-password":        hash,
		sqliteCloudflareTokenPurpose: "synthetic-cloudflare-token-" + uuid.NewString(),
		"internal-ca-private-key":    "-----BEGIN PRIVATE KEY-----\nsynthetic-ca-" + uuid.NewString() + "\n-----END PRIVATE KEY-----",
		sqliteCertificateKeyPurpose:  "-----BEGIN PRIVATE KEY-----\nsynthetic-cert-" + uuid.NewString() + "\n-----END PRIVATE KEY-----",
		"node-credential":            "synthetic-node-credential-" + uuid.NewString(),
	} {
		id, err := store.Put(context.Background(), purpose, plaintext)
		if err != nil {
			t.Fatal(err)
		}
		parsed := uuid.MustParse(id)
		ti.primarySecrets[parsed] = plaintext
		if purpose == sqliteCertificateKeyPurpose {
			ti.certSecretID = parsed
		}
	}
	return ti
}

// export builds the envelope with the real exporter over the primary's
// secrets, wrapped with the primary's cluster KEK.
func (ti *twoInstalls) export(t *testing.T) replicationsnapshot.Envelope {
	t.Helper()
	cfg := withCertificateSecret(materializeConfig(), ti.certSecretID.String())
	kek := clusterKEK(t, ti.primary)
	defer kek.Destroy()
	keyRef := ti.primary.clusterKey
	var env replicationsnapshot.Envelope
	err := ti.primaryStore.WithCanonicalSnapshot(context.Background(), func(tx CanonicalSnapshotTransaction) error {
		var err error
		env, err = replicationsnapshot.NewExporter(fixedExportConfig{snapshot: cfg}, tx, tx).Export(context.Background(), replicationsnapshot.ExporterInput{
			InstallationID:       domain.InstallationID(ti.primary.primaryID),
			NodeID:               domain.NodeID(ti.primary.nodeID),
			Role:                 domain.TopologyRolePrimary,
			Ingress:              cfg.Settings.Ingress,
			LeadershipGeneration: 7,
			ClusterKEK:           kek,
			ClusterKeyRef:        &keyRef,
		})
		return err
	})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	return env
}

// deliverKEK installs the primary's cluster KEK on the node under the primary's
// key id, the way enrollment hands it over.
func (ti *twoInstalls) deliverKEK(t *testing.T) *cluster.KEK {
	t.Helper()
	material, err := cluster.NewSQLiteKeyStore(ti.primary.db, ti.primary.writer, ti.primary.master).
		LoadActive(context.Background(), ti.primary.clusterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer material.Destroy()
	raw := material.CopyBytes()
	nodeKeys := cluster.NewSQLiteKeyStore(ti.node.db, ti.node.writer, ti.node.master)
	if err := ti.node.writer.Write(context.Background(), func(v sqlitedb.WriteValue) error {
		return nodeKeys.ImportInWrite(v, ti.primary.clusterKey, raw)
	}); err != nil {
		t.Fatal(err)
	}
	kek, err := cluster.NewKEK(raw)
	if err != nil {
		t.Fatal(err)
	}
	return kek
}

func (ti *twoInstalls) importOnNode(env replicationsnapshot.Envelope) error {
	importer := replicationsnapshot.NewImporter(ti.nodeStore, ti.nodeStore, nil)
	_, err := importer.Import(context.Background(), &env, replicationsnapshot.ImporterInput{
		LocalInstallationID: domain.InstallationID(ti.node.primaryID),
		LocalNodeID:         domain.NodeID(ti.node.nodeID),
		LocalIngress:        domain.Ingress{IPv4: materializeNodeIPv4, IPv6: materializeNodeIPv6},
	})
	return err
}

func TestTwoIndependentInstallsImportReplicatedSecretsWithNodeMasterKey(t *testing.T) {
	ti := newTwoInstalls(t)
	nodeLocalID, err := secrets.NewSQLiteStore(ti.node.db, ti.node.master).Put(context.Background(), "node-credential", "node-only-credential")
	if err != nil {
		t.Fatal(err)
	}

	env := ti.export(t)
	if len(env.Replicated.Secrets) != len(ti.primarySecrets) {
		t.Fatalf("exported %d secrets, want %d", len(env.Replicated.Secrets), len(ti.primarySecrets))
	}
	for _, s := range env.Replicated.Secrets {
		if s.Digest == "" {
			t.Fatalf("exported secret %s has no digest", s.ID)
		}
	}
	nodeKEK := ti.deliverKEK(t)
	defer nodeKEK.Destroy()
	if result := replicationsnapshot.NewValidator(nodeKEK).Validate(context.Background(), env); !result.OK {
		t.Fatalf("node validation failed: %+v", result.Issues)
	}

	if err := ti.importOnNode(env); err != nil {
		t.Fatalf("import with independent keys failed: %v", err)
	}

	nodeSecrets := secrets.NewSQLiteStore(ti.node.db, ti.node.master)
	for id, want := range ti.primarySecrets {
		got, err := nodeSecrets.Get(context.Background(), id.String())
		if err != nil || got != want {
			t.Fatalf("secret %s not readable with the node master (err %v)", id, err)
		}
		var ciphertext, purpose string
		if err := ti.node.db.QueryRow(`select ciphertext, purpose from secrets where id = ?`, id.String()).Scan(&ciphertext, &purpose); err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(purpose) == "" || strings.Contains(ciphertext, want) {
			t.Fatalf("secret %s stored badly (purpose %q)", id, purpose)
		}
		if _, err := secrets.DecryptSecret(ciphertext, ti.primary.master); err == nil {
			t.Fatalf("secret %s must not be readable with the primary master", id)
		}
	}
	if got, err := nodeSecrets.Get(context.Background(), nodeLocalID); err != nil || got != "node-only-credential" {
		t.Fatalf("node-local secret must be untouched, got %q, %v", got, err)
	}
	if n := phase2Count(t, ti.node, `select count(*) from certificates where key_secret_id = ?`, ti.certSecretID.String()); n != 1 {
		t.Fatalf("certificate must reference the imported key secret, got %d", n)
	}
}

func TestTwoIndependentInstallsLegacyEmptyPurposeSecretFailsClosedWithDiagnostic(t *testing.T) {
	ti := newTwoInstalls(t)
	legacyID := uuid.New()
	ciphertext, err := secrets.EncryptSecret("legacy-value", ti.primary.master)
	if err != nil {
		t.Fatal(err)
	}
	settingsExec(t, ti.primary.db, `insert into secrets (id, purpose, ciphertext, created_at, updated_at) values (?,?,?,'0','0')`,
		legacyID.String(), "", ciphertext)

	env := ti.export(t)
	index := -1
	for i, s := range env.Replicated.Secrets {
		if s.ID == legacyID {
			index = i
		}
	}
	if index < 0 {
		t.Fatal("legacy secret must be exported")
	}
	nodeKEK := ti.deliverKEK(t)
	defer nodeKEK.Destroy()
	result := replicationsnapshot.NewValidator(nodeKEK).Validate(context.Background(), env)
	var codes []string
	for _, issue := range result.Issues {
		codes = append(codes, issue.Code)
	}
	if result.OK || !strings.Contains(strings.Join(codes, ","), "SECRET_PURPOSE") {
		t.Fatalf("validator must flag the empty purpose, got %v", codes)
	}

	secretsBefore := phase2Count(t, ti.node, `select count(*) from secrets`)
	err = ti.importOnNode(env)
	if !errors.Is(err, ErrImportSecretPurposeMissing) {
		t.Fatalf("err = %v, want ErrImportSecretPurposeMissing", err)
	}
	if want := fmt.Sprintf("(index %d)", index); !strings.Contains(err.Error(), want) {
		t.Fatalf("error must name %s: %q", want, err)
	}
	if strings.Contains(err.Error(), legacyID.String()) || strings.Contains(err.Error(), "legacy-value") {
		t.Fatalf("diagnostic leaks values: %q", err)
	}
	if strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("diagnostic must name the exact reason, got %q", err)
	}
	if n := phase2Count(t, ti.node, `select count(*) from secrets`); n != secretsBefore {
		t.Fatalf("failed import must roll back secrets: %d -> %d", secretsBefore, n)
	}
}
