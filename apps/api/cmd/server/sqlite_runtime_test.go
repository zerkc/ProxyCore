package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/config"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
)

func sqliteTestConfig(t *testing.T) config.Config {
	t.Helper()
	return config.Config{
		Addr:               "127.0.0.1:0",
		EnrollmentTLSAddr:  "127.0.0.1:0",
		SQLitePath:         filepath.Join(t.TempDir(), "nested", "proxycore.sqlite"),
		MasterKeyBase64:    base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x21}, 32)),
		SessionCookieName:  "proxycore_session",
		SessionTTL:         time.Hour,
		UpdateCheckEnabled: false,
	}
}

func TestRunServerFailsFastWithoutSQLitePath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := runServer(ctx, config.Config{Addr: "127.0.0.1:0"}, log.New(&runtimeTestLog{}, "", 0))
	if err == nil {
		t.Fatal("runServer without PROXYCORE_SQLITE_PATH must fail fast")
	}
	if !errors.Is(err, errSQLitePathRequired) {
		t.Fatalf("error = %v, want errSQLitePathRequired", err)
	}
}

func TestSQLiteEnrollmentTokenListerAdaptsRecords(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	consumed := now.Add(time.Minute)
	source := fakeTokenListSource{records: []configuration.EnrollmentTokenListRecord{{
		ID: "id-1", Selector: "sel-1", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		ConsumedAt: &consumed, AttemptCount: 3, LastAttemptAt: &consumed,
	}}}
	list := sqliteEnrollmentTokenLister(source)
	got, err := list(context.Background(), "owner-1")
	if err != nil {
		t.Fatal(err)
	}
	if source.owner != nil && *source.owner != "owner-1" {
		t.Fatalf("owner = %q", *source.owner)
	}
	want := httpserver.EnrollmentTokenRecord{
		ID: "id-1", Selector: "sel-1", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		ConsumedAt: &consumed, AttemptCount: 3, LastAttemptAt: &consumed,
	}
	if len(got) != 1 || got[0].ID != want.ID || got[0].Selector != want.Selector || !got[0].CreatedAt.Equal(now) ||
		!got[0].ExpiresAt.Equal(want.ExpiresAt) || got[0].ConsumedAt != &consumed || got[0].RevokedAt != nil ||
		got[0].AttemptCount != 3 || got[0].LastAttemptAt != &consumed {
		t.Fatalf("adapted = %+v want %+v", got, want)
	}
}

func TestSQLiteEnrollmentTokenListerReturnsNonNilEmptyAndRejectsBadInput(t *testing.T) {
	list := sqliteEnrollmentTokenLister(fakeTokenListSource{})
	got, err := list(context.Background(), "owner")
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty = %#v err=%v", got, err)
	}
	if _, err := list(context.Background(), "  "); err == nil {
		t.Fatal("blank owner accepted")
	}
	if _, err := list(nil, "owner"); err == nil {
		t.Fatal("nil context accepted")
	}
	failing := sqliteEnrollmentTokenLister(fakeTokenListSource{err: errors.New("boom")})
	if _, err := failing(context.Background(), "owner"); err == nil {
		t.Fatal("source error swallowed")
	}
	if _, err := sqliteEnrollmentTokenLister(nil)(context.Background(), "owner"); err == nil {
		t.Fatal("nil source accepted")
	}
}

type fakeTokenListSource struct {
	records []configuration.EnrollmentTokenListRecord
	err     error
	owner   *string
}

func (f fakeTokenListSource) ListEnrollmentTokens(_ context.Context, ownerID string) ([]configuration.EnrollmentTokenListRecord, error) {
	if f.owner != nil {
		*f.owner = ownerID
	}
	return f.records, f.err
}

func TestEnrollmentTLSConfigurationAdapterTreatsTypedNilSQLiteStoreAsUnconfigured(t *testing.T) {
	for name, store := range map[string]enrollmentTLSConfigurationStore{
		"sqlite backend": (*configuration.SQLiteConfigurationBackend)(nil),
		"other pointer":  (*typedNilTLSStore)(nil),
	} {
		adapter := &enrollmentTLSConfigurationAdapter{store: store}
		if _, err := adapter.GetEnrollmentHostnames(context.Background()); !errors.Is(err, enrollment.ErrEnrollmentTLSConfigurationUnavailable) {
			t.Fatalf("%s: GetEnrollmentHostnames error = %v", name, err)
		}
		if _, err := adapter.EnsureEnrollmentTLSMaterial(context.Background()); !errors.Is(err, enrollment.ErrEnrollmentTLSMaterialUnavailable) {
			t.Fatalf("%s: EnsureEnrollmentTLSMaterial error = %v", name, err)
		}
	}
}

func TestOpenSQLiteStackBuildsWithOnlyASQLitePath(t *testing.T) {
	cfg := sqliteTestConfig(t)
	stack, err := openSQLiteStack(context.Background(), cfg, log.New(&runtimeTestLog{}, "", 0))
	if err != nil {
		t.Fatalf("openSQLiteStack: %v", err)
	}
	defer stack.Close()
	if stack.identity == nil || stack.phase2 == nil || stack.config == nil || stack.keys == nil ||
		stack.tokenAuthority == nil || stack.localKEKLoader == nil {
		t.Fatalf("incomplete stack: %+v", stack)
	}
	if stack.identityResult.InstallationID == "" || stack.identityResult.NodeID == "" {
		t.Fatalf("identity not bootstrapped: %+v", stack.identityResult)
	}
	handler := httpserver.New(cfg, nil, stack.options(cfg)...).Handler()
	if handler == nil {
		t.Fatal("handler is nil")
	}
	// stack.options does not include backup (see backupOptions); the handler
	// must still build and serve without it.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if rec.Code >= 500 {
		t.Fatalf("health status = %d", rec.Code)
	}
	// Reopening the same file is idempotent: same identity, no migration error.
	again, err := openSQLiteStack(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close()
	if again.identityResult.InstallationID != stack.identityResult.InstallationID {
		t.Fatal("identity changed across reopen")
	}
}

func TestRunServerStartsAndStopsOnSQLiteOnly(t *testing.T) {
	cfg := sqliteTestConfig(t)
	out := &runtimeTestLog{}
	logger := log.New(out, "", 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runServer(ctx, cfg, logger) }()
	if !out.WaitFor("proxycore-api listening", 10*time.Second) {
		cancel()
		t.Fatalf("server never reported listening; log=%s", out.String())
	}
	if !out.WaitFor("identity ready", time.Second) {
		t.Fatalf("identity not logged; log=%s", out.String())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runServer: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("runServer did not stop")
	}
}

// typedNilTLSStore proves the guard is not limited to the known store types.
type typedNilTLSStore struct{ called bool }

func (s *typedNilTLSStore) GetEnrollmentHostnames(context.Context) (configuration.EnrollmentHostnameConfig, error) {
	s.called = true
	return configuration.EnrollmentHostnameConfig{}, nil
}

func (s *typedNilTLSStore) EnsureEnrollmentTLSMaterial(context.Context) (configuration.EnrollmentTLSMaterial, error) {
	s.called = true
	return configuration.EnrollmentTLSMaterial{}, nil
}

func TestSQLiteStackBackupOptionsRequireMasterKey(t *testing.T) {
	cfg := sqliteTestConfig(t)
	stack, err := openSQLiteStack(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("openSQLiteStack: %v", err)
	}
	defer stack.Close()

	json := func(h http.Handler, path string, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	creds := `{"username":"owner","password":"correct horse battery staple"}`
	base := httpserver.New(cfg, nil, stack.options(cfg)...).Handler()
	if rec := json(base, "/api/auth/bootstrap", creds, nil); rec.Code != http.StatusCreated {
		t.Fatalf("bootstrap status = %d body=%s", rec.Code, rec.Body.String())
	}
	login := json(base, "/api/auth/login", creds, nil)
	cookies := login.Result().Cookies()
	if login.Code != http.StatusOK || len(cookies) != 1 {
		t.Fatalf("login status = %d cookies=%v", login.Code, cookies)
	}
	post := func(opts []httpserver.Option, path string) int {
		options := append(stack.options(cfg), opts...)
		return json(httpserver.New(cfg, nil, options...).Handler(), path, "", cookies[0]).Code
	}

	if got := post(nil, "/api/backup/export"); got != http.StatusServiceUnavailable {
		t.Fatalf("unwired export status = %d, want 503", got)
	}

	noKey := cfg
	noKey.MasterKeyBase64 = ""
	if opts := stack.backupOptions(noKey); len(opts) != 0 {
		t.Fatalf("backupOptions without master key = %d options, want 0", len(opts))
	}

	opts := stack.backupOptions(cfg)
	if len(opts) != 1 {
		t.Fatalf("backupOptions with master key = %d options, want 1", len(opts))
	}
	for _, path := range []string{"/api/backup/export", "/api/backup/import"} {
		if got := post(opts, path); got == http.StatusServiceUnavailable {
			t.Fatalf("%s still reports the backup service as unavailable", path)
		}
	}
}
