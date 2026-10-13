package configuration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// Synthetic fixture values only; keys and PEM bodies are placeholders.
const (
	certIDA = "cccccccc-0000-4000-8000-00000000000a"
	certIDB = "cccccccc-0000-4000-8000-00000000000b"
	certIDC = "cccccccc-0000-4000-8000-00000000000c"
	certIDD = "cccccccc-0000-4000-8000-00000000000d"
	certRec = "eeeeeeee-0000-4000-8000-0000000000c1"
	certMsg = "Certificate is still assigned to one or more proxied DNS records"
)

// Unix seconds around PostgreSQL's 2000-01-01 epoch (946684800) and Unix zero.
const (
	secPre1970   = int64(-1)
	secPre2000   = int64(946684799)
	secEpoch2000 = int64(946684800)
	secPost2000  = int64(946684801)
)

type certFixture struct {
	db      *sql.DB
	store   *SQLiteCertificateStore
	secrets *secrets.SQLiteStore
}

func newCertFixture(t *testing.T) *certFixture {
	t.Helper()
	f := newRecordFixture(t)
	// newRecordFixture seeds one uploaded certificate for record tests; start empty.
	settingsExec(t, f.db, `delete from certificates`)
	return &certFixture{db: f.db, store: NewSQLiteCertificateStore(f.db, settingsWriter(f.db)), secrets: f.secrets}
}

func certKey(t *testing.T, sec int64, nanos int32) *string {
	t.Helper()
	key, err := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: sec, Nanos: nanos})
	if err != nil {
		t.Fatal(err)
	}
	return key
}

type certSeed struct {
	id, hostnames, issuer, challenge, status string
	created                                  int64
	expires, renew                           *string
	secret, pem, failure                     any
}

func (f *certFixture) seed(t *testing.T, s certSeed) {
	t.Helper()
	if s.hostnames == "" {
		s.hostnames = `["a.example.test"]`
	}
	settingsExec(t, f.db, `insert into certificates (id,hostnames,issuer,challenge,environment,status,expires_at,renew_after,
		key_secret_id,certificate_pem,created_at,updated_at,failure_reason) values (?,?,?,?,'custom',?,?,?,?,?,?,?,?)`,
		s.id, s.hostnames, s.issuer, s.challenge, s.status, s.expires, s.renew, s.secret, s.pem,
		*certKey(t, s.created, 0), *certKey(t, s.created, 0), s.failure)
}

func (f *certFixture) ids(t *testing.T) []string {
	t.Helper()
	rows, err := f.db.Query(`select id from certificates order by id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

func certIDs(certs []domain.CertificateStatus) []string {
	out := []string{}
	for _, cert := range certs {
		out = append(out, cert.ID)
	}
	return out
}

func TestSQLiteCertificateListEmpty(t *testing.T) {
	f := newCertFixture(t)
	got, err := f.store.ListCertificates(context.Background())
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("want non-nil empty slice: %#v, %v", got, err)
	}
	raw, _ := json.Marshal(got)
	if string(raw) != "[]" {
		t.Fatalf("json %s", raw)
	}
}

func TestSQLiteCertificateListOrdersNewestFirstAcrossYear2000(t *testing.T) {
	f := newCertFixture(t)
	// Inserted in scrambled order so neither rowid nor id order can pass.
	f.seed(t, certSeed{id: certIDB, issuer: "uploaded", challenge: "none", status: "issued", created: secEpoch2000})
	f.seed(t, certSeed{id: certIDD, issuer: "uploaded", challenge: "none", status: "issued", created: secPre1970})
	f.seed(t, certSeed{id: certIDA, issuer: "uploaded", challenge: "none", status: "issued", created: secPost2000})
	f.seed(t, certSeed{id: certIDC, issuer: "uploaded", challenge: "none", status: "issued", created: secPre2000})
	got, err := f.store.ListCertificates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{certIDA, certIDB, certIDC, certIDD}
	if !reflect.DeepEqual(certIDs(got), want) {
		t.Fatalf("order %v want %v", certIDs(got), want)
	}
}

func TestSQLiteCertificateScannerRoundTrip(t *testing.T) {
	f := newCertFixture(t)
	secretID, err := f.secrets.Put(context.Background(), "certificate-private-key", "synthetic-key")
	if err != nil {
		t.Fatal(err)
	}
	f.seed(t, certSeed{id: certIDA, hostnames: `["a.example.test","*.example.test"]`, issuer: "letsencrypt", challenge: "dns-01",
		status: "active", created: secPost2000, expires: certKey(t, secPre2000, 123456789), renew: certKey(t, secPre1970, 5),
		secret: secretID, pem: "synthetic PEM", failure: "previous failure"})
	f.seed(t, certSeed{id: certIDB, hostnames: `[]`, issuer: "self-signed", challenge: "none", status: "pending", created: secPre2000})
	got, err := f.store.ListCertificates(context.Background())
	if err != nil || len(got) != 2 {
		t.Fatalf("%v %v", got, err)
	}
	full, bare := got[0], got[1]
	if full.ID != certIDA || !reflect.DeepEqual(full.Hostnames, []string{"a.example.test", "*.example.test"}) ||
		full.Issuer != "letsencrypt" || full.Challenge != "dns-01" || full.Environment != "custom" || full.Status != "active" ||
		full.SecretID != secretID || full.CertificatePEM != "synthetic PEM" || full.FailureReason != "previous failure" {
		t.Fatalf("full row %+v", full)
	}
	if full.ExpiresAt == nil || full.ExpiresAt.Unix() != secPre2000 || full.ExpiresAt.Nanosecond() != 123456789 ||
		full.RenewAfter == nil || full.RenewAfter.Unix() != secPre1970 || full.RenewAfter.Nanosecond() != 5 {
		t.Fatalf("clocks %v %v", full.ExpiresAt, full.RenewAfter)
	}
	if bare.ExpiresAt != nil || bare.RenewAfter != nil || bare.SecretID != "" || bare.CertificatePEM != "" ||
		bare.FailureReason != "" || len(bare.Hostnames) != 0 {
		t.Fatalf("null row %+v", bare)
	}
}

func TestSQLiteCertificateListRejectsNonFiniteClock(t *testing.T) {
	f := newCertFixture(t)
	f.seed(t, certSeed{id: certIDA, issuer: "uploaded", challenge: "none", status: "issued", created: secPost2000, renew: ptr("2")})
	if got, err := f.store.ListCertificates(context.Background()); err == nil {
		t.Fatalf("infinity decoded silently: %+v", got)
	}
}

func ptr(s string) *string { return &s }

func TestSQLiteCertificateGet(t *testing.T) {
	f := newCertFixture(t)
	f.seed(t, certSeed{id: certIDA, issuer: "uploaded", challenge: "none", status: "issued", created: secPost2000, pem: "synthetic PEM"})
	ctx := context.Background()
	for _, id := range []string{certIDA, strings.ToUpper(certIDA), strings.ReplaceAll(certIDA, "-", ""), "{" + certIDA + "}"} {
		got, err := f.store.GetCertificate(ctx, id)
		if err != nil || got.ID != certIDA || got.CertificatePEM != "synthetic PEM" {
			t.Fatalf("get %q: %+v %v", id, got, err)
		}
	}
	got, err := f.store.GetCertificate(ctx, certIDB)
	if !errors.Is(err, ErrCertificateNotFound) || err.Error() != "Certificate not found" || !reflect.DeepEqual(got, domain.CertificateStatus{}) {
		t.Fatalf("missing: %+v %v", got, err)
	}
	// PostgreSQL text observed on postgres:17: id echoed raw, SQLSTATE 22P02.
	for _, id := range []string{"nope", "", certIDA + " ", "{" + certIDA} {
		_, err := f.store.GetCertificate(ctx, id)
		want := fmt.Sprintf(`ERROR: invalid input syntax for type uuid: "%s" (SQLSTATE 22P02)`, id)
		if err == nil || err.Error() != want || errors.Is(err, ErrCertificateNotFound) {
			t.Fatalf("malformed %q: %v", id, err)
		}
	}
}

func (f *certFixture) seedReference(t *testing.T, enabled int, certificateID any) {
	t.Helper()
	proxy := any(nil)
	if certificateID != nil {
		raw, _ := json.Marshal(map[string]any{"tlsEnabled": true, "certificateId": certificateID})
		proxy = string(raw)
	}
	settingsExec(t, f.db, `insert into dns_records (id,zone_id,name,type,value,ttl,enabled,proxied,proxy_settings,created_at,updated_at)
		values (?,?,'www.example.test','A','"192.0.2.10"',300,?,1,?,'0','0')`, certRec, recordZoneID, enabled, proxy)
}

func (f *certFixture) state(t *testing.T) string {
	t.Helper()
	return recordState(t, f.db) + fmt.Sprint(f.ids(t))
}

func TestSQLiteCertificateDeleteSuccessKeepsKeySecret(t *testing.T) {
	f := newCertFixture(t)
	secretID, err := f.secrets.Put(context.Background(), "certificate-private-key", "synthetic-key")
	if err != nil {
		t.Fatal(err)
	}
	f.seed(t, certSeed{id: certIDA, issuer: "uploaded", challenge: "none", status: "issued", created: secPost2000, secret: secretID})
	f.seed(t, certSeed{id: certIDB, issuer: "uploaded", challenge: "none", status: "issued", created: secPost2000})
	// A different certificate and an unrelated reference must not block.
	f.seedReference(t, 1, certIDB)
	if err := f.store.DeleteCertificate(context.Background(), strings.ToUpper(certIDA)); err != nil {
		t.Fatal(err)
	}
	if got := f.ids(t); !reflect.DeepEqual(got, []string{certIDB}) {
		t.Fatalf("remaining %v", got)
	}
	// PG parity: the key secret is intentionally left behind.
	// Secrets.Get returns "" for a missing row, so assert the plaintext itself.
	if plain, err := f.secrets.Get(context.Background(), secretID); err != nil || plain != "synthetic-key" {
		t.Fatalf("key secret must survive certificate deletion: %q %v", plain, err)
	}
	var rows int
	if err := f.db.QueryRow(`select count(*) from secrets where id = ?`, secretID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("secret row count %d %v", rows, err)
	}
}

func TestSQLiteCertificateDeleteNotFoundAndMalformed(t *testing.T) {
	f := newCertFixture(t)
	f.seed(t, certSeed{id: certIDA, issuer: "uploaded", challenge: "none", status: "issued", created: secPost2000})
	before := f.state(t)
	err := f.store.DeleteCertificate(context.Background(), certIDB)
	if !errors.Is(err, ErrCertificateNotFound) || err.Error() != "Certificate not found" {
		t.Fatalf("missing: %v", err)
	}
	err = f.store.DeleteCertificate(context.Background(), "nope")
	if err == nil || err.Error() != `ERROR: invalid input syntax for type uuid: "nope" (SQLSTATE 22P02)` {
		t.Fatalf("malformed: %v", err)
	}
	if f.state(t) != before {
		t.Fatal("state changed")
	}
}

func TestSQLiteCertificateDeleteInUseCountsEveryRecordAndRollsBack(t *testing.T) {
	for name, enabled := range map[string]int{"enabled": 1, "disabled records still count (PG has no enabled filter)": 0} {
		t.Run(name, func(t *testing.T) {
			f := newCertFixture(t)
			f.seed(t, certSeed{id: certIDA, issuer: "uploaded", challenge: "none", status: "issued", created: secPost2000})
			f.seedReference(t, enabled, certIDA)
			before := f.state(t)
			for _, id := range []string{certIDA, strings.ToUpper(certIDA), strings.ReplaceAll(certIDA, "-", "")} {
				err := f.store.DeleteCertificate(context.Background(), id)
				if !errors.Is(err, ErrCertificateInUse) || err.Error() != certMsg {
					t.Fatalf("%q: %v", id, err)
				}
				if f.state(t) != before {
					t.Fatal("state changed after in-use error")
				}
			}
		})
	}
}

// PG compares the stored text with the raw argument, so a non-canonical spelling
// stored inside proxy_settings never matches. The SQLite adapter pins that
// exact-text behavior (AddRecord only stores ids found in the certificate map).
func TestSQLiteCertificateDeleteIgnoresNonCanonicalStoredReference(t *testing.T) {
	for name, ref := range map[string]any{"upper": strings.ToUpper(certIDA), "number": 7, "null": nil, "other": certIDB} {
		t.Run(name, func(t *testing.T) {
			f := newCertFixture(t)
			f.seed(t, certSeed{id: certIDA, issuer: "uploaded", challenge: "none", status: "issued", created: secPost2000})
			f.seedReference(t, 1, ref)
			if err := f.store.DeleteCertificate(context.Background(), certIDA); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("record without proxy settings", func(t *testing.T) {
		f := newCertFixture(t)
		f.seed(t, certSeed{id: certIDA, issuer: "uploaded", challenge: "none", status: "issued", created: secPost2000})
		f.seedReference(t, 1, nil)
		settingsExec(t, f.db, `update dns_records set proxied=0`)
		if err := f.store.DeleteCertificate(context.Background(), certIDA); err != nil {
			t.Fatal(err)
		}
	})
}

// DeleteCertificate runs select, reference count and delete in ONE writer
// transaction, so a record created concurrently either sees the certificate
// (and AddRecord then succeeds before the delete, which reports in-use) or the
// certificate is gone first (AddRecord fails). PG's separate statements allowed
// a dangling reference; this must never happen here.
func TestSQLiteCertificateConcurrentDeleteVersusRecordReference(t *testing.T) {
	f := newCertFixture(t)
	records := NewSQLiteRecordWriteStore(settingsWriter(f.db), f.secrets)
	for i := 0; i < 25; i++ {
		id := fmt.Sprintf("cccccccc-0000-4000-8000-0000000001%02d", i)
		f.seed(t, certSeed{id: id, issuer: "uploaded", challenge: "none", status: "issued", created: secPost2000, hostnames: `["*.example.test"]`})
		input := recordProxied(nil)
		input.Name = fmt.Sprintf("w%d", i)
		input.Proxy["certificateId"] = id
		var wg sync.WaitGroup
		var delErr, addErr error
		start := make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); <-start; delErr = f.store.DeleteCertificate(context.Background(), id) }()
		go func() {
			defer wg.Done()
			<-start
			_, addErr = records.AddRecord(context.Background(), recordZoneID, input, "")
		}()
		close(start)
		wg.Wait()
		var exists, refs int
		_ = f.db.QueryRow(`select count(*) from certificates where id=?`, id).Scan(&exists)
		_ = f.db.QueryRow(`select count(*) from dns_records where json_extract(proxy_settings,'$.certificateId')=?`, id).Scan(&refs)
		if exists == 0 && refs != 0 {
			t.Fatalf("iteration %d: dangling reference (delete=%v add=%v)", i, delErr, addErr)
		}
		if exists == 1 && delErr == nil {
			t.Fatalf("iteration %d: delete succeeded but certificate remains", i)
		}
		if exists == 1 && refs == 1 && !errors.Is(delErr, ErrCertificateInUse) {
			t.Fatalf("iteration %d: delete=%v", i, delErr)
		}
	}
}

func TestSQLiteCertificateCanceledContextAndClosedDatabase(t *testing.T) {
	f := newCertFixture(t)
	f.seed(t, certSeed{id: certIDA, issuer: "uploaded", challenge: "none", status: "issued", created: secPost2000})
	before := f.state(t)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	calls := map[string]func(context.Context) error{
		"list":   func(c context.Context) error { _, err := f.store.ListCertificates(c); return err },
		"get":    func(c context.Context) error { _, err := f.store.GetCertificate(c, certIDA); return err },
		"delete": func(c context.Context) error { return f.store.DeleteCertificate(c, certIDA) },
		"due": func(c context.Context) error {
			_, err := f.store.ListCertificatesDueForRenewal(c, time.Now())
			return err
		},
	}
	for name, call := range calls {
		if err := call(canceled); !errors.Is(err, context.Canceled) {
			t.Fatalf("%s canceled: %v", name, err)
		}
	}
	if f.state(t) != before {
		t.Fatal("canceled delete changed state")
	}
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	for name, call := range calls {
		if err := call(context.Background()); err == nil {
			t.Fatalf("%s accepted a closed database", name)
		}
	}
}

type dueCase struct {
	name, issuer, challenge, status string
	renew                           *string
	due                             bool
}

// dueMatrix is shared with the PG parity test; renew values are Unix seconds
// relative to dueNow and encoded per engine.
var dueNow = time.Date(2030, 6, 1, 12, 0, 0, 0, time.UTC)

func dueMatrix() []struct {
	dueCase
	renewSec *int64
} {
	at := func(delta int64) *int64 { v := dueNow.Unix() + delta; return &v }
	pre2000, post2000 := secPre2000, secPost2000
	mk := func(name, issuer, challenge, status string, renew *int64, due bool) struct {
		dueCase
		renewSec *int64
	} {
		return struct {
			dueCase
			renewSec *int64
		}{dueCase{name: name, issuer: issuer, challenge: challenge, status: status, due: due}, renew}
	}
	return []struct {
		dueCase
		renewSec *int64
	}{
		mk("le-http-before", "letsencrypt", "http-01", "active", at(-10), true),
		mk("le-dns-equal-now", "letsencrypt", "dns-01", "active", at(0), true),
		mk("le-dns-after", "letsencrypt", "dns-01", "active", at(1), false),
		mk("ss-none-oldest", "self-signed", "none", "active", at(-1000), true),
		mk("le-none-wrong-challenge", "letsencrypt", "none", "active", at(-10), false),
		mk("ss-http-wrong-challenge", "self-signed", "http-01", "active", at(-10), false),
		mk("uploaded-none", "uploaded", "none", "active", at(-10), false),
		mk("le-pending", "letsencrypt", "http-01", "pending", at(-10), false),
		mk("le-issued", "letsencrypt", "http-01", "issued", at(-10), false),
		mk("le-expired", "letsencrypt", "http-01", "expired", at(-10), false),
		mk("le-failed", "letsencrypt", "http-01", "failed", at(-10), false),
		mk("le-null-renew", "letsencrypt", "http-01", "active", nil, false),
		mk("ss-pre2000-renew", "self-signed", "none", "active", &pre2000, true),
		mk("ss-post2000-renew", "self-signed", "none", "active", &post2000, true),
		mk("ss-pre1970-renew", "self-signed", "none", "active", func() *int64 { v := secPre1970; return &v }(), true),
	}
}

func dueID(i int) string { return fmt.Sprintf("dddddddd-0000-4000-8000-%012d", i) }

func TestSQLiteCertificateListDueForRenewalMatrix(t *testing.T) {
	f := newCertFixture(t)
	cases := dueMatrix()
	for i, c := range cases {
		var renew *string
		if c.renewSec != nil {
			renew = certKey(t, *c.renewSec, 0)
		}
		f.seed(t, certSeed{id: dueID(i), issuer: c.issuer, challenge: c.challenge, status: c.status, created: secPost2000, renew: renew})
	}
	got, err := f.store.ListCertificatesDueForRenewal(context.Background(), dueNow)
	if err != nil {
		t.Fatal(err)
	}
	want := dueOrder(cases)
	if !reflect.DeepEqual(certIDs(got), want) {
		t.Fatalf("due %v want %v", certIDs(got), want)
	}
	// A local-zone clock must select the same rows as UTC.
	zone := time.FixedZone("x", -7*3600)
	got, err = f.store.ListCertificatesDueForRenewal(context.Background(), dueNow.In(zone))
	if err != nil || !reflect.DeepEqual(certIDs(got), want) {
		t.Fatalf("zone clock %v %v", certIDs(got), err)
	}
	empty, err := f.store.ListCertificatesDueForRenewal(context.Background(), time.Unix(secPre1970-5, 0))
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("far past: %#v %v", empty, err)
	}
}

// dueOrder returns expected ids sorted by renew_after ascending.
func dueOrder(cases []struct {
	dueCase
	renewSec *int64
}) []string {
	var idx []int
	for i, c := range cases {
		if c.due {
			idx = append(idx, i)
		}
	}
	for a := 1; a < len(idx); a++ {
		for b := a; b > 0 && *cases[idx[b]].renewSec < *cases[idx[b-1]].renewSec; b-- {
			idx[b], idx[b-1] = idx[b-1], idx[b]
		}
	}
	out := []string{}
	for _, i := range idx {
		out = append(out, dueID(i))
	}
	return out
}

func (f *certFixture) write(t *testing.T, fn func(sqlitedb.WriteValue) error) error {
	t.Helper()
	return f.store.writer.Write(context.Background(), fn)
}

func TestSQLiteCertificateInsertActiveFailedAndPending(t *testing.T) {
	f := newCertFixture(t)
	secretID, err := f.secrets.Put(context.Background(), "certificate-private-key", "synthetic-key")
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Unix(secPre2000, 42).UTC()
	renew := time.Unix(secPost2000, 7).UTC()
	var active, failed, pending domain.CertificateStatus
	err = f.write(t, func(v sqlitedb.WriteValue) error {
		var err error
		active, err = sqliteInsertCertificate(v, domain.CertificateStatus{ID: strings.ToUpper(certIDA), Hostnames: []string{"a.example.test"},
			Issuer: "letsencrypt", Challenge: "http-01", Environment: "staging", Status: "active", ExpiresAt: &expires,
			RenewAfter: &renew, SecretID: secretID, CertificatePEM: "synthetic PEM"})
		if err != nil {
			return err
		}
		failed, err = sqliteInsertCertificate(v, domain.CertificateStatus{ID: certIDB, Hostnames: []string{"b.example.test"},
			Issuer: "letsencrypt", Challenge: "dns-01", Environment: "staging", Status: "failed", FailureReason: "challenge failed"})
		if err != nil {
			return err
		}
		pending, err = sqliteInsertCertificate(v, domain.CertificateStatus{ID: certIDC, Hostnames: []string{"c"}, Issuer: "self-signed",
			Challenge: "none", Environment: "local"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if active.ID != certIDA || active.Status != "active" || active.SecretID != secretID || !active.ExpiresAt.Equal(expires) ||
		!active.RenewAfter.Equal(renew) || active.CertificatePEM != "synthetic PEM" || active.FailureReason != "" {
		t.Fatalf("active %+v", active)
	}
	if failed.Status != "failed" || failed.FailureReason != "challenge failed" || failed.ExpiresAt != nil || failed.RenewAfter != nil || failed.SecretID != "" {
		t.Fatalf("failed %+v", failed)
	}
	if pending.Status != "pending" {
		t.Fatalf("PG defaults an empty status to pending: %+v", pending)
	}
	stamp, _ := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: 123, Nanos: 456})
	var created, updated string
	if err := f.db.QueryRow(`select created_at, updated_at from certificates where id=?`, certIDA).Scan(&created, &updated); err != nil ||
		created != *stamp || updated != *stamp {
		t.Fatalf("stamps %q %q %v", created, updated, err)
	}
	// Round trip through the read path.
	if got, err := f.store.GetCertificate(context.Background(), certIDA); err != nil || !reflect.DeepEqual(got, active) {
		t.Fatalf("round trip %+v vs %+v (%v)", got, active, err)
	}
}

func TestSQLiteCertificateInsertRejectsBadInputAndRollsBack(t *testing.T) {
	f := newCertFixture(t)
	base := domain.CertificateStatus{ID: certIDA, Hostnames: []string{"a"}, Issuer: "uploaded", Challenge: "none", Environment: "custom", Status: "issued"}
	before := f.state(t)
	for name, mutate := range map[string]func(*domain.CertificateStatus){
		"malformed id":     func(c *domain.CertificateStatus) { c.ID = "nope" },
		"malformed secret": func(c *domain.CertificateStatus) { c.SecretID = "nope" },
		"missing secret":   func(c *domain.CertificateStatus) { c.SecretID = certIDD },
		"bad issuer":       func(c *domain.CertificateStatus) { c.Issuer = "acme" },
		"bad status":       func(c *domain.CertificateStatus) { c.Status = "weird" },
	} {
		c := base
		mutate(&c)
		err := f.write(t, func(v sqlitedb.WriteValue) error { _, err := sqliteInsertCertificate(v, c); return err })
		if err == nil {
			t.Fatalf("%s accepted", name)
		}
		if name == "malformed id" && err.Error() != `ERROR: invalid input syntax for type uuid: "nope" (SQLSTATE 22P02)` {
			t.Fatalf("text %v", err)
		}
	}
	if f.state(t) != before {
		t.Fatal("state changed")
	}
	// Duplicate id fails like PG's primary key.
	f.seed(t, certSeed{id: certIDA, issuer: "uploaded", challenge: "none", status: "issued", created: secPost2000})
	if err := f.write(t, func(v sqlitedb.WriteValue) error { _, err := sqliteInsertCertificate(v, base); return err }); err == nil {
		t.Fatal("duplicate id accepted")
	}
}

func TestSQLiteCertificateUpdateActiveMaterial(t *testing.T) {
	f := newCertFixture(t)
	ctx := context.Background()
	oldSecret, _ := f.secrets.Put(ctx, "certificate-private-key", "old")
	newSecret, _ := f.secrets.Put(ctx, "certificate-private-key", "new")
	f.seed(t, certSeed{id: certIDA, issuer: "letsencrypt", challenge: "http-01", status: "active", created: secPre2000,
		expires: certKey(t, 1, 0), renew: certKey(t, 1, 0), secret: oldSecret, pem: "old PEM", failure: "stale failure"})
	f.seed(t, certSeed{id: certIDB, issuer: "letsencrypt", challenge: "http-01", status: "failed", created: secPre2000})
	f.seed(t, certSeed{id: certIDC, issuer: "self-signed", challenge: "none", status: "active", created: secPre2000})
	expires := time.Unix(secPost2000+1000, 9).UTC()
	renew := time.Unix(secPost2000, 3).UTC()
	update := func(id, issuer string) (domain.CertificateStatus, error) {
		var got domain.CertificateStatus
		err := f.write(t, func(v sqlitedb.WriteValue) error {
			var err error
			got, err = sqliteUpdateActiveCertificateMaterial(v, id, issuer, newSecret, "new PEM", expires, renew)
			return err
		})
		return got, err
	}
	got, err := update(strings.ToUpper(certIDA), "letsencrypt")
	if err != nil || got.ID != certIDA || got.Status != "active" || got.SecretID != newSecret || got.CertificatePEM != "new PEM" ||
		got.FailureReason != "" || !got.ExpiresAt.Equal(expires) || !got.RenewAfter.Equal(renew) {
		t.Fatalf("update %+v %v", got, err)
	}
	var created, updated string
	_ = f.db.QueryRow(`select created_at, updated_at from certificates where id=?`, certIDA).Scan(&created, &updated)
	stamp, _ := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: 123, Nanos: 456})
	if updated != *stamp || created != *certKey(t, secPre2000, 0) {
		t.Fatalf("updated_at must move, created_at must not: %q %q", created, updated)
	}
	for name, args := range map[string][2]string{"not active": {certIDB, "letsencrypt"}, "issuer mismatch": {certIDC, "letsencrypt"}, "missing": {certIDD, "letsencrypt"}} {
		before := f.state(t)
		got, err := update(args[0], args[1])
		if err == nil || err.Error() != fmt.Sprintf("certificate %s is no longer active", args[0]) || !reflect.DeepEqual(got, domain.CertificateStatus{}) {
			t.Fatalf("%s: %+v %v", name, got, err)
		}
		if f.state(t) != before {
			t.Fatalf("%s changed state", name)
		}
	}
	if _, err := update("nope", "letsencrypt"); err == nil || err.Error() != `ERROR: invalid input syntax for type uuid: "nope" (SQLSTATE 22P02)` {
		t.Fatalf("malformed: %v", err)
	}
}

// SQLite-only: the infinity sentinels and sub-second boundaries cannot be
// expressed in the PG comparison matrix above. '2' (+infinity) is never due and
// does not hide other rows; '0' (-infinity) is selected but fails to decode, so
// the whole list errors, like PG's -infinity due list.
func TestSQLiteCertificateDueSentinelsAndSubSecondBoundaries(t *testing.T) {
	ctx := context.Background()
	f := newCertFixture(t)
	nowSec := dueNow.Unix()
	f.seed(t, certSeed{id: dueID(1), issuer: "self-signed", challenge: "none", status: "active", created: secPost2000, renew: certKey(t, nowSec, 0)})
	f.seed(t, certSeed{id: dueID(2), issuer: "self-signed", challenge: "none", status: "active", created: secPost2000, renew: certKey(t, nowSec-1, 999_999_999)})
	f.seed(t, certSeed{id: dueID(3), issuer: "self-signed", challenge: "none", status: "active", created: secPost2000, renew: certKey(t, nowSec, 1)})
	f.seed(t, certSeed{id: dueID(4), issuer: "self-signed", challenge: "none", status: "active", created: secPost2000, renew: certKey(t, nowSec+1, 0)})
	positive := "2"
	f.seed(t, certSeed{id: dueID(5), issuer: "self-signed", challenge: "none", status: "active", created: secPost2000, renew: &positive})

	got, err := f.store.ListCertificatesDueForRenewal(ctx, dueNow)
	if err != nil || !reflect.DeepEqual(certIDs(got), []string{dueID(2), dueID(1)}) {
		t.Fatalf("sub-second boundaries at now: %v %v", certIDs(got), err)
	}
	got, err = f.store.ListCertificatesDueForRenewal(ctx, dueNow.Add(time.Nanosecond))
	if err != nil || !reflect.DeepEqual(certIDs(got), []string{dueID(2), dueID(1), dueID(3)}) {
		t.Fatalf("now plus 1ns: %v %v", certIDs(got), err)
	}
	got, err = f.store.ListCertificatesDueForRenewal(ctx, dueNow.Add(-time.Nanosecond))
	if err != nil || !reflect.DeepEqual(certIDs(got), []string{dueID(2)}) {
		t.Fatalf("now minus 1ns: %v %v", certIDs(got), err)
	}

	negative := "0"
	f.seed(t, certSeed{id: dueID(6), issuer: "self-signed", challenge: "none", status: "active", created: secPost2000, renew: &negative})
	got, err = f.store.ListCertificatesDueForRenewal(ctx, dueNow)
	if err == nil || got != nil {
		t.Fatalf("a -infinity renew_after must fail the list: %v %v", certIDs(got), err)
	}
}
