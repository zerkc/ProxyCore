package configuration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/acme"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

func sqliteEnrollmentConfigure(t *testing.T, f *caFixture, names []string) {
	t.Helper()
	s := NewSQLiteSettingsStore(f.db, f.store.writer, domain.Ingress{})
	if _, err := s.UpdateEnrollmentHostnames(t.Context(), names); err != nil {
		t.Fatal(err)
	}
}

func sqliteEnrollmentSeed(t *testing.T) (*caFixture, EnrollmentTLSMaterial, string, string) {
	t.Helper()
	f := newCAFixture(t)
	sqliteEnrollmentConfigure(t, f, []string{"enroll.example"})
	material, err := f.store.EnsureEnrollmentTLSMaterial(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var caID, leafID string
	if err := f.db.QueryRow(`select key_secret_id, enrollment_key_secret_id from internal_ca`).Scan(&caID, &leafID); err != nil {
		t.Fatal(err)
	}
	return f, material, caID, leafID
}

// Fingerprint every persisted column, including timestamps and ciphertext, but
// never print those synthetic secret values on assertion failure.
func sqliteEnrollmentPersistence(t *testing.T, db *sql.DB) [32]byte {
	t.Helper()
	var all []any
	for _, table := range []string{"installation_settings", "internal_ca", "internal_ca_enrollment_state", "secrets"} {
		rows, err := db.Query(`select * from ` + table + ` order by id`)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			dest := make([]any, len(columns))
			for i := range values {
				dest[i] = &values[i]
			}
			if err := rows.Scan(dest...); err != nil {
				t.Fatal(err)
			}
			all = append(all, values)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	raw, err := json.Marshal(all)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(raw)
}

func sqliteEnrollmentUnchanged(t *testing.T, f *caFixture, before [32]byte) {
	t.Helper()
	if sqliteEnrollmentPersistence(t, f.db) != before {
		t.Fatal("operation unexpectedly mutated persisted enrollment state")
	}
}

func TestSQLiteEnrollmentTLSConfiguredNotReadyIsReadOnly(t *testing.T) {
	f := newCAFixture(t)
	before := sqliteEnrollmentPersistence(t, f.db)
	trust, err := f.store.LoadEnrollmentTLSTrust(t.Context())
	if err != nil || trust.Configured || trust.Ready {
		t.Fatal("missing settings should be unconfigured", err)
	}
	sqliteEnrollmentUnchanged(t, f, before)
	sqliteEnrollmentConfigure(t, f, []string{"enroll.example"})
	before = sqliteEnrollmentPersistence(t, f.db)
	trust, err = f.store.LoadEnrollmentTLSTrust(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !trust.Configured || trust.Ready || trust.Material != (EnrollmentTLSPublicMaterial{}) {
		t.Fatal("expected configured but not ready trust")
	}
	sqliteEnrollmentUnchanged(t, f, before)
	f.assertEmpty(t)
}

func TestSQLiteEnrollmentTLSIssuesMaterial(t *testing.T) {
	f, material, caID, leafID := sqliteEnrollmentSeed(t)
	if material.PrivateKeyPEM == "" || material.CertificatePEM == "" || material.CACertificatePEM == "" {
		t.Fatal("missing enrollment material")
	}
	ca := parseEnrollmentCertificate(t, material.CACertificatePEM)
	if material.CADERHashSHA256 != acme.DERHashSHA256(ca.Raw) {
		t.Fatal("incorrect CA DER fingerprint")
	}
	for _, value := range []any{material, material.Public()} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"PRIVATE KEY", "PrivateKeyPEM", caID, leafID, "ciphertext"} {
			if strings.Contains(string(raw), forbidden) {
				t.Fatal("serialized enrollment material leaked secret data")
			}
		}
	}
	var purpose, ciphertext, caStamp, markerStamp string
	if err := f.db.QueryRow(`select purpose, ciphertext from secrets where id=?`, leafID).Scan(&purpose, &ciphertext); err != nil {
		t.Fatal(err)
	}
	if purpose != EnrollmentLeafKeyPurpose || ciphertext == material.PrivateKeyPEM || strings.Contains(ciphertext, "PRIVATE KEY") {
		t.Fatal("leaf key is not stored with encrypted enrollment purpose")
	}
	if err := f.db.QueryRow(`select updated_at from internal_ca`).Scan(&caStamp); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow(`select established_at from internal_ca_enrollment_state`).Scan(&markerStamp); err != nil {
		t.Fatal(err)
	}
	if caStamp != markerStamp || caStamp != *certKey(t, 123, 456) {
		t.Fatal("atomic write timestamps differ")
	}
	if f.count(t, `select count(*) from secrets`) != 2 {
		t.Fatal("expected one CA and one leaf key secret")
	}
	cert := parseEnrollmentCertificate(t, material.CertificatePEM)
	if !within(cert.NotAfter, time.Now().Add(365*24*time.Hour), time.Minute) {
		t.Fatal("leaf validity is not 365 days")
	}
}

func TestSQLiteEnrollmentTLSReloadsExistingPublicTrust(t *testing.T) {
	db, _, path := sqliteSettingsFixture(t)
	sec := secrets.NewSQLiteStore(db, recordMasterKey)
	f := &caFixture{db: db, secrets: sec, store: NewSQLiteInternalCAStore(db, settingsWriter(db), sec)}
	sqliteEnrollmentConfigure(t, f, []string{"enroll.example"})
	first, err := f.store.EnsureEnrollmentTLSMaterial(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	before := sqliteEnrollmentPersistence(t, db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	store := NewSQLiteInternalCAStore(reopened, settingsWriter(reopened), secrets.NewSQLiteStore(reopened, recordMasterKey))
	second, err := store.EnsureEnrollmentTLSMaterial(t.Context())
	if err != nil || first != second {
		t.Fatal("reopened ensure changed enrollment identity", err)
	}
	trust, err := store.LoadEnrollmentTLSTrust(t.Context())
	if err != nil || !trust.Configured || !trust.Ready || trust.Material != first.Public() {
		t.Fatal("reopened trust differs from issued public projection", err)
	}
	if sqliteEnrollmentPersistence(t, reopened) != before {
		t.Fatal("reload mutated persistence")
	}
}

func TestSQLiteEnrollmentTLSIssuesAndKeepsSPKI(t *testing.T) {
	f, first, caID, leafID := sqliteEnrollmentSeed(t)
	caKey, err := f.secrets.Get(t.Context(), caID)
	if err != nil {
		t.Fatal(err)
	}
	short, err := acme.IssueSignedByCAWithKey([]string{"enroll.example"}, 1, first.CACertificatePEM, caKey, first.PrivateKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	settingsExec(t, f.db, `update internal_ca set enrollment_certificate_pem=?`, short.CertificatePEM)
	renewed, err := f.store.EnsureEnrollmentTLSMaterial(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if renewed.CertificatePEM == short.CertificatePEM || !bytes.Equal(enrollmentSPKI(t, parseEnrollmentCertificate(t, first.CertificatePEM)), enrollmentSPKI(t, parseEnrollmentCertificate(t, renewed.CertificatePEM))) {
		t.Fatal("near-expiry renewal did not retain SPKI")
	}
	sqliteEnrollmentConfigure(t, f, []string{"enroll.example", "192.0.2.10"})
	before := sqliteEnrollmentPersistence(t, f.db)
	if _, err := f.store.LoadEnrollmentTLSTrust(t.Context()); err == nil {
		t.Fatal("trust accepted stale configured SANs")
	}
	sqliteEnrollmentUnchanged(t, f, before)
	updated, err := f.store.EnsureEnrollmentTLSMaterial(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	cert := parseEnrollmentCertificate(t, updated.CertificatePEM)
	if updated.PrivateKeyPEM != first.PrivateKeyPEM || !bytes.Equal(enrollmentSPKI(t, cert), enrollmentSPKI(t, parseEnrollmentCertificate(t, first.CertificatePEM))) || len(cert.DNSNames) != 1 || cert.DNSNames[0] != "enroll.example" || len(cert.IPAddresses) != 1 || !cert.IPAddresses[0].Equal(net.ParseIP("192.0.2.10")) {
		t.Fatal("SAN renewal changed leaf identity or omitted SANs")
	}
	var persistedCAID, persistedLeafID string
	if err := f.db.QueryRow(`select key_secret_id, enrollment_key_secret_id from internal_ca`).Scan(&persistedCAID, &persistedLeafID); err != nil {
		t.Fatal(err)
	}
	if caID != persistedCAID || leafID != persistedLeafID || first.CACertificatePEM != updated.CACertificatePEM || f.count(t, `select count(*) from secrets`) != 2 {
		t.Fatal("renewal rotated CA or key secrets")
	}
}

func TestSQLiteEnrollmentTLSCAOnlyIsNotReady(t *testing.T) {
	f := newCAFixture(t)
	sqliteEnrollmentConfigure(t, f, []string{"enroll.example"})
	if _, err := f.store.EnsureInternalCACertificatePEM(t.Context()); err != nil {
		t.Fatal(err)
	}
	before := sqliteEnrollmentPersistence(t, f.db)
	trust, err := f.store.LoadEnrollmentTLSTrust(t.Context())
	if err != nil || !trust.Configured || trust.Ready || trust.Material != (EnrollmentTLSPublicMaterial{}) {
		t.Fatal("CA-only should not establish enrollment trust", err)
	}
	sqliteEnrollmentUnchanged(t, f, before)
	if _, err := f.store.EnsureEnrollmentTLSMaterial(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.count(t, `select count(*) from secrets`) != 2 {
		t.Fatal("CA-only ensure duplicated the CA key")
	}
}

func TestSQLiteEnrollmentTLSFailsClosed(t *testing.T) {
	for _, name := range []string{"missing CA key", "corrupt CA", "tampered CA signature", "missing leaf key", "deleted internal CA", "cleared enrollment leaf", "wrong key", "wrong chain", "bad EKU", "expired leaf", "malformed SAN", "missing establishment marker"} {
		t.Run(name, func(t *testing.T) {
			f, first, caID, leafID := sqliteEnrollmentSeed(t)
			switch name {
			case "missing CA key":
				settingsExec(t, f.db, `update secrets set ciphertext='bad' where id=?`, caID)
			case "corrupt CA":
				settingsExec(t, f.db, `update internal_ca set certificate_pem='bad'`)
			case "tampered CA signature":
				block, _ := pem.Decode([]byte(first.CACertificatePEM))
				block.Bytes[len(block.Bytes)-1] ^= 1
				settingsExec(t, f.db, `update internal_ca set certificate_pem=?`, string(pem.EncodeToMemory(block)))
			case "missing leaf key":
				settingsExec(t, f.db, `update secrets set ciphertext='bad' where id=?`, leafID)
			case "deleted internal CA":
				settingsExec(t, f.db, `delete from internal_ca`)
			case "cleared enrollment leaf":
				settingsExec(t, f.db, `update internal_ca set enrollment_certificate_pem=null, enrollment_key_secret_id=null`)
			case "wrong key", "wrong chain":
				other, err := acme.CreateInternalCA(3650)
				if err != nil {
					t.Fatal(err)
				}
				if name == "wrong key" {
					wrongID, err := f.secrets.Put(t.Context(), EnrollmentLeafKeyPurpose, other.PrivateKeyPEM)
					if err != nil {
						t.Fatal(err)
					}
					settingsExec(t, f.db, `update internal_ca set enrollment_key_secret_id=?`, wrongID)
				} else {
					leaf, err := acme.IssueSignedByCAWithKey([]string{"enroll.example"}, 365, other.CertificatePEM, other.PrivateKeyPEM, first.PrivateKeyPEM)
					if err != nil {
						t.Fatal(err)
					}
					settingsExec(t, f.db, `update internal_ca set enrollment_certificate_pem=?`, leaf.CertificatePEM)
				}
			case "bad EKU", "expired leaf", "malformed SAN":
				caKey, err := f.secrets.Get(t.Context(), caID)
				if err != nil {
					t.Fatal(err)
				}
				bad := variantCertificate(t, first.CACertificatePEM, caKey, first.CertificatePEM, name == "expired leaf", name == "malformed SAN")
				settingsExec(t, f.db, `update internal_ca set enrollment_certificate_pem=?`, bad)
			case "missing establishment marker":
				settingsExec(t, f.db, `delete from internal_ca_enrollment_state`)
			}
			before := sqliteEnrollmentPersistence(t, f.db)
			if _, err := f.store.EnsureEnrollmentTLSMaterial(t.Context()); err == nil {
				t.Fatal("ensure accepted established tamper")
			}
			sqliteEnrollmentUnchanged(t, f, before)
			if _, err := f.store.LoadEnrollmentTLSTrust(t.Context()); err == nil {
				t.Fatal("trust accepted established tamper")
			}
			sqliteEnrollmentUnchanged(t, f, before)
		})
	}
}

func TestSQLiteEnrollmentTLSRejectsNonCanonicalStoredLeafPEM(t *testing.T) {
	for _, name := range []string{"appended certificate", "appended private key", "trailing garbage", "leading whitespace"} {
		t.Run(name, func(t *testing.T) {
			f, first, _, _ := sqliteEnrollmentSeed(t)
			stored := first.CertificatePEM
			switch name {
			case "appended certificate":
				stored += "\n" + first.CertificatePEM
			case "appended private key":
				stored += "\n" + first.PrivateKeyPEM
			case "trailing garbage":
				stored += "\ngarbage"
			case "leading whitespace":
				stored = " " + stored
			}
			settingsExec(t, f.db, `update internal_ca set enrollment_certificate_pem=?`, stored)
			before := sqliteEnrollmentPersistence(t, f.db)
			if _, err := f.store.LoadEnrollmentTLSTrust(t.Context()); err == nil {
				t.Fatal("accepted non-canonical leaf PEM")
			}
			if _, err := f.store.EnsureEnrollmentTLSMaterial(t.Context()); err == nil {
				t.Fatal("ensure repaired non-canonical leaf PEM")
			}
			sqliteEnrollmentUnchanged(t, f, before)
		})
	}
}

func TestSQLiteEnrollmentTLSCanonicalizesStoredLeafPEM(t *testing.T) {
	f, first, _, _ := sqliteEnrollmentSeed(t)
	stored := strings.TrimRight(first.CertificatePEM, "\r\n") + "\n \t\r\n"
	settingsExec(t, f.db, `update internal_ca set enrollment_certificate_pem=?`, stored)
	before := sqliteEnrollmentPersistence(t, f.db)
	trust, err := f.store.LoadEnrollmentTLSTrust(t.Context())
	if err != nil || !trust.Ready || trust.Material.CertificatePEM != first.CertificatePEM {
		t.Fatal("public leaf projection is not canonical", err)
	}
	sqliteEnrollmentUnchanged(t, f, before)
}

func TestSQLiteEnrollmentTLSHostnameReadPurity(t *testing.T) {
	for _, tc := range []struct {
		name                string
		raw                 any
		configured, invalid bool
	}{
		{name: "SQL NULL"}, {name: "JSON null", raw: "null"}, {name: "empty array", raw: "[]"},
		{name: "canonical names", raw: `["ENROLL.example.","192.0.2.10","enroll.example"]`, configured: true},
		{name: "invalid hostname", raw: `["*.example"]`, invalid: true},
		{name: "wrong JSON shape", raw: `{"host":"example"}`, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCAFixture(t)
			sqliteEnrollmentConfigure(t, f, []string{"enroll.example"})
			settingsExec(t, f.db, `update installation_settings set enrollment_hostnames=?`, tc.raw)
			before := sqliteEnrollmentPersistence(t, f.db)
			trust, err := f.store.LoadEnrollmentTLSTrust(t.Context())
			if tc.invalid {
				if err == nil || !strings.Contains(err.Error(), "read enrollment hostnames:") {
					t.Fatal("hostname validation error lost read prefix")
				}
			} else if err != nil || trust.Configured != tc.configured || trust.Ready {
				t.Fatal("unexpected pure hostname trust state", err)
			}
			sqliteEnrollmentUnchanged(t, f, before)
			// The exact same helper must work with the caller's write connection.
			if err := f.store.writer.Write(t.Context(), func(v sqlitedb.WriteValue) error {
				cfg, err := sqliteEnrollmentHostnames(v.Context(), v.Conn())
				if tc.invalid {
					if err == nil {
						return errors.New("invalid names accepted")
					}
					return nil
				}
				if err != nil {
					return err
				}
				if cfg.Configured != tc.configured {
					return errors.New("transactional hostname read differs")
				}
				if tc.configured && strings.Join(cfg.Hostnames, ",") != "192.0.2.10,enroll.example" {
					return errors.New("names not normalized")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			sqliteEnrollmentUnchanged(t, f, before)
		})
	}
}

func TestSQLiteEnrollmentTLSRequiresConfigurationAndMasterKey(t *testing.T) {
	f := newCAFixture(t)
	before := sqliteEnrollmentPersistence(t, f.db)
	if _, err := f.store.EnsureEnrollmentTLSMaterial(t.Context()); err == nil || err.Error() != "enrollment hostnames must be configured before issuing TLS material" {
		t.Fatal("missing hostname guard", err)
	}
	sqliteEnrollmentUnchanged(t, f, before)
	for _, sec := range []sqliteInternalCASecrets{nil, (*secrets.SQLiteStore)(nil)} {
		store := NewSQLiteInternalCAStore(f.db, f.store.writer, sec)
		if _, err := store.EnsureEnrollmentTLSMaterial(t.Context()); err == nil || err.Error() != "enrollment TLS requires a master key" {
			t.Fatal("missing master key guard", err)
		}
	}
	f, _, _, _ = sqliteEnrollmentSeed(t)
	before = sqliteEnrollmentPersistence(t, f.db)
	store := NewSQLiteInternalCAStore(f.db, f.store.writer, nil)
	if _, err := store.LoadEnrollmentTLSTrust(t.Context()); err == nil || err.Error() != "established enrollment TLS material is unavailable" {
		t.Fatal("missing established trust key guard", err)
	}
	wrong := NewSQLiteInternalCAStore(f.db, f.store.writer, secrets.NewSQLiteStore(f.db, "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="))
	if _, err := wrong.LoadEnrollmentTLSTrust(t.Context()); err == nil {
		t.Fatal("wrong master key accepted")
	}
	if _, err := wrong.EnsureEnrollmentTLSMaterial(t.Context()); err == nil {
		t.Fatal("wrong master key repaired enrollment")
	}
	sqliteEnrollmentUnchanged(t, f, before)
}

func TestSQLiteEnrollmentTLSRollback(t *testing.T) {
	for _, stage := range []string{"CA insert", "leaf update", "marker insert", "renewal update", "secret insert"} {
		t.Run(stage, func(t *testing.T) {
			f := newCAFixture(t)
			sqliteEnrollmentConfigure(t, f, []string{"enroll.example"})
			var trigger string
			switch stage {
			case "CA insert":
				trigger = `create trigger reject_ca before insert on internal_ca begin select raise(abort,'test CA failure'); end`
			case "leaf update":
				trigger = `create trigger reject_leaf before update on internal_ca begin select raise(abort,'test leaf failure'); end`
			case "marker insert":
				trigger = `create trigger reject_marker before insert on internal_ca_enrollment_state begin select raise(abort,'test marker failure'); end`
			case "secret insert":
				trigger = `create trigger reject_secret before insert on secrets when new.purpose='internal-ca-enrollment-leaf-private-key' begin select raise(abort,'test secret failure'); end`
			case "renewal update":
				if _, err := f.store.EnsureEnrollmentTLSMaterial(t.Context()); err != nil {
					t.Fatal(err)
				}
				sqliteEnrollmentConfigure(t, f, []string{"new.example"})
				trigger = `create trigger reject_renewal before update on internal_ca begin select raise(abort,'test renewal failure'); end`
			}
			settingsExec(t, f.db, trigger)
			before := sqliteEnrollmentPersistence(t, f.db)
			material, err := f.store.EnsureEnrollmentTLSMaterial(t.Context())
			if err == nil || material != (EnrollmentTLSMaterial{}) {
				t.Fatal("failed write returned candidate or succeeded")
			}
			sqliteEnrollmentUnchanged(t, f, before)
		})
	}
}

func TestSQLiteEnrollmentTLSConcurrentCallersConverge(t *testing.T) {
	f := newCAFixture(t)
	sqliteEnrollmentConfigure(t, f, []string{"enroll.example"})
	const callers = 6
	start := make(chan struct{})
	results := make(chan EnrollmentTLSMaterial, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			m, err := f.store.EnsureEnrollmentTLSMaterial(t.Context())
			results <- m
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first EnrollmentTLSMaterial
	for m := range results {
		if first.CertificatePEM == "" {
			first = m
		} else if first != m {
			t.Fatal("concurrent callers returned different identities")
		}
	}
	if f.count(t, `select count(*) from internal_ca`) != 1 || f.count(t, `select count(*) from internal_ca_enrollment_state`) != 1 || f.count(t, `select count(*) from secrets`) != 2 {
		t.Fatal("concurrent ensure left duplicate/orphan state")
	}
}

// Signal completion of the pure CA read while another writer owns the database.
// No global hooks or sleeps: the held writer publishes a deterministic winner
// only after every competing ensure has observed the CA-only snapshot.
type sqliteEnrollmentReadBarrier struct {
	sqliteInternalCASecrets
	read chan struct{}
}

func (s *sqliteEnrollmentReadBarrier) Get(ctx context.Context, id string) (string, error) {
	key, err := s.sqliteInternalCASecrets.Get(ctx, id)
	if err == nil {
		s.read <- struct{}{}
	}
	return key, err
}

func TestSQLiteEnrollmentTLSHeldWriterFirstWinner(t *testing.T) {
	f := newCAFixture(t)
	sqliteEnrollmentConfigure(t, f, []string{"enroll.example"})
	ca, err := f.store.EnsureInternalCACertificatePEM(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var caID string
	if err := f.db.QueryRow(`select key_secret_id from internal_ca`).Scan(&caID); err != nil {
		t.Fatal(err)
	}
	key, err := f.secrets.Get(t.Context(), caID)
	if err != nil {
		t.Fatal(err)
	}
	winner, err := acme.IssueSignedByCAWithKey([]string{"enroll.example"}, 365, ca, key, "")
	if err != nil {
		t.Fatal(err)
	}
	const callers = 4
	barrier := &sqliteEnrollmentReadBarrier{sqliteInternalCASecrets: f.secrets, read: make(chan struct{}, callers)}
	store := NewSQLiteInternalCAStore(f.db, f.store.writer, barrier)
	held := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- f.store.writer.Write(t.Context(), func(v sqlitedb.WriteValue) error {
			close(held)
			select {
			case <-release:
			case <-v.Context().Done():
				return v.Context().Err()
			}
			id, err := f.secrets.PutInWrite(v, EnrollmentLeafKeyPurpose, winner.PrivateKeyPEM)
			if err != nil {
				return err
			}
			if _, err := v.Conn().ExecContext(v.Context(), `update internal_ca set enrollment_certificate_pem=?, enrollment_key_secret_id=?`, winner.CertificatePEM, id); err != nil {
				return err
			}
			_, err = v.Conn().ExecContext(v.Context(), `insert into internal_ca_enrollment_state (id, established_at) values (?, ?)`, enrollmentStateID, v.Timestamp())
			return err
		})
	}()
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("writer did not acquire transaction")
	}
	readCtx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	trust, err := f.store.LoadEnrollmentTLSTrust(readCtx)
	if err != nil || !trust.Configured || trust.Ready {
		t.Fatal("trust load acquired or waited for the held writer", err)
	}
	results := make(chan EnrollmentTLSMaterial, callers)
	errs := make(chan error, callers)
	for range callers {
		go func() { m, err := store.EnsureEnrollmentTLSMaterial(t.Context()); results <- m; errs <- err }()
	}
	for range callers {
		select {
		case <-barrier.read:
		case <-time.After(10 * time.Second):
			t.Fatal("pure CA read blocked behind held writer")
		}
	}
	unblock()
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
	for range callers {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("ensure blocked: possible nested writer")
		}
		m := <-results
		if m.CertificatePEM != winner.CertificatePEM || m.PrivateKeyPEM != winner.PrivateKeyPEM {
			t.Fatal("queued writer did not return committed winner")
		}
	}
	if f.count(t, `select count(*) from secrets`) != 2 {
		t.Fatal("losing writers orphaned key candidates")
	}
}

func TestSQLiteEnrollmentTLSHeldWriterRechecksHostnames(t *testing.T) {
	for _, clear := range []bool{false, true} {
		name := "changed"
		if clear {
			name = "cleared"
		}
		t.Run(name, func(t *testing.T) {
			f := newCAFixture(t)
			sqliteEnrollmentConfigure(t, f, []string{"enroll.example"})
			if _, err := f.store.EnsureInternalCACertificatePEM(t.Context()); err != nil {
				t.Fatal(err)
			}
			barrier := &sqliteEnrollmentReadBarrier{sqliteInternalCASecrets: f.secrets, read: make(chan struct{}, 8)}
			store := NewSQLiteInternalCAStore(f.db, f.store.writer, barrier)
			held, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			done := make(chan error, 1)
			go func() {
				done <- f.store.writer.Write(t.Context(), func(v sqlitedb.WriteValue) error {
					close(held)
					select {
					case <-release:
					case <-v.Context().Done():
						return v.Context().Err()
					}
					var raw any = `["changed.example"]`
					if clear {
						raw = nil
					}
					_, err := v.Conn().ExecContext(v.Context(), `update installation_settings set enrollment_hostnames=?`, raw)
					return err
				})
			}()
			select {
			case <-held:
			case <-time.After(10 * time.Second):
				t.Fatal("writer acquisition timed out")
			}
			results := make(chan EnrollmentTLSMaterial, 1)
			errs := make(chan error, 1)
			go func() { m, err := store.EnsureEnrollmentTLSMaterial(t.Context()); results <- m; errs <- err }()
			select {
			case <-barrier.read:
			case <-time.After(10 * time.Second):
				t.Fatal("hostname candidate read blocked")
			}
			unblock()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			var err error
			select {
			case err = <-errs:
			case <-time.After(10 * time.Second):
				t.Fatal("hostname recheck deadlocked")
			}
			m := <-results
			if clear {
				if err == nil || m != (EnrollmentTLSMaterial{}) || f.count(t, `select count(*) from secrets`) != 1 || f.count(t, `select count(*) from internal_ca_enrollment_state`) != 0 {
					t.Fatal("cleared names allowed stale candidate persistence")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if !sameEnrollmentHostnames(parseEnrollmentCertificate(t, m.CertificatePEM), []string{"changed.example"}) || f.count(t, `select count(*) from secrets`) != 2 {
					t.Fatal("changed names did not discard stale candidate")
				}
			}
		})
	}
}

func TestSQLiteEnrollmentTLSOneAtomicWrite(t *testing.T) {
	f := newCAFixture(t)
	sqliteEnrollmentConfigure(t, f, []string{"enroll.example"})
	writes := 0
	f.store.writer = sqlitedb.NewWriter(f.db, func() sqlitedb.FiniteTimestamp {
		writes++
		return sqlitedb.FiniteTimestamp{Seconds: 123, Nanos: 456}
	})
	first, err := f.store.EnsureEnrollmentTLSMaterial(t.Context())
	if err != nil || writes != 1 {
		t.Fatal("first establishment must use one write", err, writes)
	}
	before := sqliteEnrollmentPersistence(t, f.db)
	second, err := f.store.EnsureEnrollmentTLSMaterial(t.Context())
	if err != nil || first != second || writes != 1 {
		t.Fatal("valid identity should use the read fast path", err, writes)
	}
	sqliteEnrollmentUnchanged(t, f, before)
	sqliteEnrollmentConfigure(t, f, []string{"changed.example"})
	writes = 0
	if _, err := f.store.EnsureEnrollmentTLSMaterial(t.Context()); err != nil || writes != 1 {
		t.Fatal("renewal must use one write", err, writes)
	}
	if f.count(t, `select count(*) from secrets`) != 2 {
		t.Fatal("renewal stored extra key secrets")
	}
}

func TestSQLiteEnrollmentTLSCanceledReadsDoNotMutate(t *testing.T) {
	f, _, _, _ := sqliteEnrollmentSeed(t)
	before := sqliteEnrollmentPersistence(t, f.db)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.store.LoadEnrollmentTLSTrust(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("trust did not propagate canceled read", err)
	}
	if _, err := f.store.EnsureEnrollmentTLSMaterial(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("ensure did not propagate cancellation", err)
	}
	sqliteEnrollmentUnchanged(t, f, before)
}
