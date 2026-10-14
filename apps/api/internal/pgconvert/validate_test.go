package pgconvert

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	googleuuid "github.com/google/uuid"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
	"github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
)

// testMasterKey is a fixed 32-byte test key; never used outside tests.
var testMasterKey = base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))

const otherMasterKey = "ZmVkY2JhOTg3NjU0MzIxMGZlZGNiYTk4NzY1NDMyMTA="

func pgExec(t *testing.T, f pgFixture, query string, args ...any) {
	t.Helper()
	if _, err := f.connect(t).Exec(context.Background(), query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

// newValidatablePG is the seeded fixture with real ciphertext (EncryptSecret and
// testMasterKey), real revision checksums and a real replication envelope, so
// the validator's cryptographic checks run against genuine values.
func newValidatablePG(t *testing.T) pgFixture {
	t.Helper()
	f := newSeededPG(t)
	for _, id := range []string{"16", "17", "18"} {
		ciphertext, err := secrets.EncryptSecret("plaintext-"+id, testMasterKey)
		if err != nil {
			t.Fatal(err)
		}
		pgExec(t, f, `update secrets set ciphertext = $1 where id = $2`, ciphertext, fixtureUUID(id))
	}
	for _, id := range []string{"32", "33"} {
		wrapped, err := secrets.EncryptSecret("kek-material-"+id, testMasterKey)
		if err != nil {
			t.Fatal(err)
		}
		pgExec(t, f, `update cluster_keys set wrapped_kek = $1 where id = $2`, wrapped, fixtureUUID(id))
	}
	for _, id := range []string{"22", "23"} {
		var raw string
		if err := f.connect(t).QueryRow(context.Background(), `select snapshot::text from config_revisions where id = $1`, fixtureUUID(id)).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var value any
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			t.Fatal(err)
		}
		pgExec(t, f, `update config_revisions set checksum = $1 where id = $2`, domain.ChecksumSnapshot(value), fixtureUUID(id))
	}
	env := snapshot.Envelope{
		Transient: snapshot.TransientFields{SnapshotVersion: domain.SnapshotVersionV1, ReplicationVersion: domain.ReplicationVersionV1,
			SourcePrimaryID: googleuuid.MustParse(fixtureUUID("30")), LeadershipGeneration: 5, CapturedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
		NodeLocal: snapshot.NodeLocalFields{NodeID: domain.NodeID(fixtureUUID("31")), Role: domain.TopologyRoleNode},
		Replicated: snapshot.ReplicatedFields{Configuration: map[string]any{"zones": []any{}},
			Secrets: []snapshot.ReplicatedSecret{}, Owners: []snapshot.ReplicatedOwner{}},
	}
	hash, err := env.ExpectedHash()
	if err != nil {
		t.Fatal(err)
	}
	env.ContentHash = hash
	body, err := snapshot.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	pgExec(t, f, `update applied_snapshots set content_hash = $1, snapshot_body = $2 where id = $3`, hash, body, fixtureUUID("34"))
	pgExec(t, f, `update applied_snapshots set snapshot_body = null where id = $1`, fixtureUUID("35"))
	return f
}

// validateFixture converts f and validates the output with a second reader of
// the same data. mutate may alter the SQLite file between convert and validate.
func validateFixture(t *testing.T, f pgFixture, key string, mutate func(db *sql.DB)) (*ValidationReport, error) {
	t.Helper()
	ctx := context.Background()
	src, err := OpenSource(ctx, f.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close(ctx)
	dest := filepath.Join(t.TempDir(), "out.db")
	if _, err := Convert(ctx, src, dest); err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if mutate != nil {
		db := openDest(t, dest)
		mutate(db)
		db.Close()
	}
	return Validate(ctx, src, dest, key)
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func failedChecks(t *testing.T, err error) map[Check]bool {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want *ValidationError, got %T: %v", err, err)
	}
	out := map[Check]bool{}
	for _, f := range ve.Failures {
		out[f.Check] = true
	}
	return out
}

func TestValidateAcceptsFaithfulConversion(t *testing.T) {
	f := newValidatablePG(t)
	report, err := validateFixture(t, f, testMasterKey, nil)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(report.Tables) != 30 {
		t.Fatalf("report has %d tables", len(report.Tables))
	}
	for name, tr := range report.Tables {
		if tr.SourceRows != tr.DestRows || tr.Hash != tr.DestHash || tr.Hash == "" {
			t.Errorf("%s: %+v", name, tr)
		}
	}
	if report.SchemaVersion != 7 {
		t.Errorf("schema version %d", report.SchemaVersion)
	}
	if report.SecretsDecrypted != 5 || report.RevisionChecksums != 2 || report.SnapshotBlobs != 1 || report.LegacySnapshots != 2 {
		t.Errorf("counters: %+v", report)
	}
}

func TestValidateRejectsFlippedCiphertextByte(t *testing.T) {
	f := newValidatablePG(t)
	_, err := validateFixture(t, f, testMasterKey, func(db *sql.DB) {
		var ct string
		if err := db.QueryRow(`select ciphertext from secrets where id = ?`, fixtureUUID("16")).Scan(&ct); err != nil {
			t.Fatal(err)
		}
		b := []byte(ct)
		b[len(b)-2] ^= 1
		mustExec(t, db, `update secrets set ciphertext = ? where id = ?`, string(b), fixtureUUID("16"))
	})
	got := failedChecks(t, err)
	if !got[CheckSecret] || !got[CheckTableHash] {
		t.Fatalf("failures: %v", got)
	}
}

func TestValidateRejectsWrongMasterKey(t *testing.T) {
	f := newValidatablePG(t)
	_, err := validateFixture(t, f, otherMasterKey, nil)
	got := failedChecks(t, err)
	if !got[CheckSecret] || len(got) != 1 {
		t.Fatalf("failures: %v", got)
	}
}

func TestValidateRejectsTamperedChecksum(t *testing.T) {
	f := newValidatablePG(t)
	_, err := validateFixture(t, f, testMasterKey, func(db *sql.DB) {
		mustExec(t, db, `update config_revisions set checksum = 'forged' where revision_number = 1`)
	})
	got := failedChecks(t, err)
	if !got[CheckRevisionChecksum] || !got[CheckTableHash] {
		t.Fatalf("failures: %v", got)
	}
}

func TestValidateRejectsCorruptedRow(t *testing.T) {
	f := newValidatablePG(t)
	_, err := validateFixture(t, f, testMasterKey, func(db *sql.DB) {
		mustExec(t, db, `update zones set name = 'tampered.test' where id = ?`, fixtureUUID("7"))
	})
	got := failedChecks(t, err)
	if !got[CheckTableHash] || got[CheckTableRows] {
		t.Fatalf("failures: %v", got)
	}
}

func TestValidateRejectsMissingRow(t *testing.T) {
	f := newValidatablePG(t)
	_, err := validateFixture(t, f, testMasterKey, func(db *sql.DB) {
		mustExec(t, db, `delete from health_observations where id = ?`, fixtureUUID("27"))
	})
	if got := failedChecks(t, err); !got[CheckTableRows] {
		t.Fatalf("failures: %v", got)
	}
}

func TestValidateRejectsTamperedSnapshotBlobAndHash(t *testing.T) {
	f := newValidatablePG(t)
	_, err := validateFixture(t, f, testMasterKey, func(db *sql.DB) {
		var body []byte
		if err := db.QueryRow(`select snapshot_body from applied_snapshots where id = ?`, fixtureUUID("34")).Scan(&body); err != nil {
			t.Fatal(err)
		}
		body = bytes.Replace(body, []byte(`"zones"`), []byte(`"zonez"`), 1)
		mustExec(t, db, `update applied_snapshots set snapshot_body = ? where id = ?`, body, fixtureUUID("34"))
	})
	got := failedChecks(t, err)
	if !got[CheckSnapshotBody] || !got[CheckSnapshotHash] {
		t.Fatalf("failures: %v", got)
	}
	f = newValidatablePG(t)
	_, err = validateFixture(t, f, testMasterKey, func(db *sql.DB) {
		mustExec(t, db, `update applied_snapshots set content_hash = 'forged' where id = ?`, fixtureUUID("34"))
	})
	if got := failedChecks(t, err); !got[CheckSnapshotHash] {
		t.Fatalf("failures: %v", got)
	}
}

func TestValidateRejectsSchemaDrift(t *testing.T) {
	f := newValidatablePG(t)
	_, err := validateFixture(t, f, testMasterKey, func(db *sql.DB) {
		mustExec(t, db, `update proxycore_schema_migrations set checksum = 'bogus' where version = 6`)
	})
	if got := failedChecks(t, err); !got[CheckSchema] {
		t.Fatalf("failures: %v", got)
	}
}

func TestValidateRejectsForeignKeyViolation(t *testing.T) {
	f := newValidatablePG(t)
	_, err := validateFixture(t, f, testMasterKey, func(db *sql.DB) {
		conn, cerr := db.Conn(context.Background())
		if cerr != nil {
			t.Fatal(cerr)
		}
		defer conn.Close()
		if _, cerr = conn.ExecContext(context.Background(), `pragma foreign_keys = off`); cerr != nil {
			t.Fatal(cerr)
		}
		if _, cerr = conn.ExecContext(context.Background(), `update dns_records set zone_id = ? where id = ?`, fixtureUUID("99"), fixtureUUID("9")); cerr != nil {
			t.Fatal(cerr)
		}
	})
	if got := failedChecks(t, err); !got[CheckForeignKeys] {
		t.Fatalf("failures: %v", got)
	}
}

func TestValidateRejectsSourceDataThatDoesNotVerify(t *testing.T) {
	// The raw seeded fixture carries fake ciphertext and fake checksums: a
	// faithful copy of unverifiable data must still fail the conversion.
	f := newSeededPG(t)
	_, err := validateFixture(t, f, testMasterKey, nil)
	got := failedChecks(t, err)
	if !got[CheckSecret] || !got[CheckRevisionChecksum] {
		t.Fatalf("failures: %v", got)
	}
}

func TestValidateMissingDestination(t *testing.T) {
	f := newValidatablePG(t)
	ctx := context.Background()
	src, err := OpenSource(ctx, f.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close(ctx)
	missing := filepath.Join(t.TempDir(), "absent.db")
	if _, err := Validate(ctx, src, missing, testMasterKey); err == nil {
		t.Fatal("expected error")
	}
	if _, statErr := os.Stat(missing); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("validator created the destination: %v", statErr)
	}
}
