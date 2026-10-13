package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/pgconvert"
	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
)

const testPGEnv = "PROXYCORE_CONVERT_TEST_PG_URL"

var testKey = base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))

const wrongKey = "ZmVkY2JhOTg3NjU0MzIxMGZlZGNiYTk4NzY1NDMyMTA="

// newTestPG creates a database owned by the test, applies the real Drizzle
// migrations and seeds a few rows, including a real ciphertext and checksum.
func newTestPG(t *testing.T) string {
	t.Helper()
	base := os.Getenv(testPGEnv)
	if base == "" {
		t.Skipf("%s not set", testPGEnv)
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	name := "cli_" + hex.EncodeToString(suffix[:])
	if _, err := admin.Exec(ctx, "create database "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), base)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), "drop database if exists "+name+" with (force)")
	})
	parsed, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + name
	conn, err := pgx.Connect(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	files, err := filepath.Glob(filepath.Join("..", "..", "internal", "pgconvert", "testdata", "pg-migrations", "*.sql"))
	if err != nil || len(files) != 9 {
		t.Fatalf("drizzle migrations: %v (%d files)", err, len(files))
	}
	sort.Strings(files)
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, statement := range strings.Split(string(raw), "--> statement-breakpoint") {
			if strings.TrimSpace(statement) == "" {
				continue
			}
			if _, err := conn.Exec(ctx, statement); err != nil {
				t.Fatalf("apply %s: %v", filepath.Base(file), err)
			}
		}
	}
	ciphertext, err := secrets.EncryptSecret("provider-token", testKey)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := `{"zones":[{"name":"example.test"}]}`
	var value any
	if err := json.Unmarshal([]byte(snapshot), &value); err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`insert into users (id, username, password_hash, role, active) values ('00000000-0000-4000-8000-000000000001','owner','h','owner',true)`, nil},
		{`insert into zones (id, name, enabled) values ('00000000-0000-4000-8000-000000000002','example.test',true)`, nil},
		{`insert into secrets (id, purpose, ciphertext) values ('00000000-0000-4000-8000-000000000003','provider',$1)`, []any{ciphertext}},
		{`insert into config_revisions (id, revision_number, checksum, snapshot) values ('00000000-0000-4000-8000-000000000004',1,$1,$2::jsonb)`,
			[]any{domain.ChecksumSnapshot(value), snapshot}},
	} {
		if _, err := conn.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	return parsed.String()
}

// pgDigest hashes every table of the source so a run can prove PG is unchanged.
func pgDigest(t *testing.T, pgURL string) map[string]string {
	t.Helper()
	ctx := context.Background()
	src, err := pgconvert.OpenSource(ctx, pgURL)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close(ctx)
	plan, err := src.Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, spec := range pgconvert.Tables() {
		rows, hash, err := pgconvert.HashPGTable(ctx, src, plan, spec.Name)
		if err != nil {
			t.Fatal(err)
		}
		out[spec.Name] = hash + ":" + string(rune('0'+rows%10))
	}
	return out
}

type result struct {
	code           int
	stdout, stderr string
}

func runCLI(t *testing.T, env map[string]string, args ...string) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, func(k string) string { return env[k] }, &stdout, &stderr)
	return result{code, stdout.String(), stderr.String()}
}

func assertNoLeftovers(t *testing.T, out string) {
	t.Helper()
	for _, suffix := range []string{".tmp", ".tmp-wal", ".tmp-shm", ".tmp-journal", "-wal", "-shm", "-journal", ".converted.json", ".converted.json.tmp"} {
		if _, err := os.Lstat(out + suffix); err == nil {
			t.Errorf("leftover file %s%s", filepath.Base(out), suffix)
		}
	}
}

func TestRunConvertsValidatesAndPublishes(t *testing.T) {
	pgURL := newTestPG(t)
	before := pgDigest(t, pgURL)
	out := filepath.Join(t.TempDir(), "proxycore.db")
	res := runCLI(t, nil, "--pg-url", pgURL, "--out", out, "--master-key-base64", testKey)
	if res.code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	if info, err := os.Stat(out); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("output: %v %v", info, err)
	}
	if _, err := os.Lstat(out + ".tmp"); err == nil {
		t.Error(".tmp left behind")
	}
	raw, err := os.ReadFile(out + ".converted.json")
	if err != nil {
		t.Fatalf("marker: %v", err)
	}
	var marker struct {
		Format        string
		ToolVersion   string `json:"tool_version"`
		ConvertedAt   string `json:"converted_at"`
		SchemaVersion int    `json:"schema_version"`
		Fingerprint   string `json:"source_schema_fingerprint"`
		Tables        map[string]struct {
			Rows int64
			Hash string
		}
		Warnings []any
	}
	if err := json.Unmarshal(raw, &marker); err != nil {
		t.Fatal(err)
	}
	if marker.Format == "" || marker.ToolVersion == "" || marker.ConvertedAt == "" || marker.SchemaVersion != 6 ||
		len(marker.Fingerprint) != 64 || len(marker.Tables) != 30 || marker.Tables["secrets"].Rows != 1 || marker.Tables["secrets"].Hash == "" {
		t.Errorf("marker: %s", raw)
	}
	for name, text := range map[string]string{"stdout": res.stdout, "stderr": res.stderr, "marker": string(raw)} {
		for _, secret := range []string{testKey, pgURL, "provider-token"} {
			if strings.Contains(text, secret) {
				t.Errorf("%s leaks a secret", name)
			}
		}
	}
	if !strings.Contains(res.stdout, "validated") {
		t.Errorf("summary: %s", res.stdout)
	}
	after := pgDigest(t, pgURL)
	for table, hash := range before {
		if after[table] != hash {
			t.Errorf("PostgreSQL table %s changed", table)
		}
	}
}

func TestRunReadsEnvironmentFallbacks(t *testing.T) {
	pgURL := newTestPG(t)
	out := filepath.Join(t.TempDir(), "proxycore.db")
	res := runCLI(t, map[string]string{"PROXYCORE_CONVERT_PG_URL": pgURL, "PROXYCORE_MASTER_KEY_BASE64": testKey}, "--out", out)
	if res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
}

func TestRunRefusesExistingOutputAndLeavesItUntouched(t *testing.T) {
	pgURL := newTestPG(t)
	out := filepath.Join(t.TempDir(), "proxycore.db")
	if err := os.WriteFile(out, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := runCLI(t, nil, "--pg-url", pgURL, "--out", out, "--master-key-base64", testKey)
	if res.code != 2 {
		t.Fatalf("exit %d, want 2: %s", res.code, res.stderr)
	}
	if got, _ := os.ReadFile(out); string(got) != "precious" {
		t.Errorf("existing output modified: %q", got)
	}
	if _, err := os.Lstat(out + ".converted.json"); err == nil {
		t.Error("marker written for refused run")
	}
	if _, err := os.Lstat(out + ".tmp"); err == nil {
		t.Error(".tmp created for refused run")
	}
}

func TestRunWrongMasterKeyFailsAndLeavesNothing(t *testing.T) {
	pgURL := newTestPG(t)
	before := pgDigest(t, pgURL)
	out := filepath.Join(t.TempDir(), "proxycore.db")
	res := runCLI(t, nil, "--pg-url", pgURL, "--out", out, "--master-key-base64", wrongKey)
	if res.code != 1 {
		t.Fatalf("exit %d, want 1\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	if _, err := os.Lstat(out); err == nil {
		t.Error("output published despite failed validation")
	}
	assertNoLeftovers(t, out)
	if strings.Contains(res.stderr, wrongKey) {
		t.Error("stderr leaks the key")
	}
	for table, hash := range pgDigest(t, pgURL) {
		if before[table] != hash {
			t.Errorf("PostgreSQL table %s changed", table)
		}
	}
}

func TestRunUnknownSourceTableIsASourceError(t *testing.T) {
	pgURL := newTestPG(t)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, pgURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `create table surprise (id int)`); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "proxycore.db")
	res := runCLI(t, nil, "--pg-url", pgURL, "--out", out, "--master-key-base64", testKey)
	if res.code != 3 {
		t.Fatalf("exit %d, want 3: %s", res.code, res.stderr)
	}
	assertNoLeftovers(t, out)
	if _, err := os.Lstat(out); err == nil {
		t.Error("output published")
	}
}

func TestRunUnreachableSourceNeverLeaksPassword(t *testing.T) {
	out := filepath.Join(t.TempDir(), "proxycore.db")
	res := runCLI(t, nil, "--pg-url", "postgres://user:Zq9-distinct-pw@127.0.0.1:1/db?sslmode=disable&connect_timeout=2", "--out", out, "--master-key-base64", testKey)
	if res.code != 3 {
		t.Fatalf("exit %d, want 3: %s", res.code, res.stderr)
	}
	if strings.Contains(res.stdout+res.stderr, "Zq9-distinct-pw") {
		t.Error("password leaked")
	}
	assertNoLeftovers(t, out)
}

func TestRunUsageErrors(t *testing.T) {
	out := filepath.Join(t.TempDir(), "proxycore.db")
	cases := map[string][]string{
		"no arguments":     {},
		"missing pg url":   {"--out", out, "--master-key-base64", testKey},
		"missing out":      {"--pg-url", "postgres://x/y", "--master-key-base64", testKey},
		"missing key":      {"--pg-url", "postgres://x/y", "--out", out},
		"short master key": {"--pg-url", "postgres://x/y", "--out", out, "--master-key-base64", "c2hvcnQ="},
		"force flag":       {"--pg-url", "postgres://x/y", "--out", out, "--master-key-base64", testKey, "--force"},
		"unknown argument": {"--pg-url", "postgres://x/y", "--out", out, "--master-key-base64", testKey, "extra"},
	}
	for name, args := range cases {
		if res := runCLI(t, nil, args...); res.code != 2 {
			t.Errorf("%s: exit %d, want 2 (%s)", name, res.code, res.stderr)
		}
	}
	assertNoLeftovers(t, out)
}
