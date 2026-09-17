package configuration

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

func TestNormalizeEnrollmentHostnamesCanonicalizesAndSorts(t *testing.T) {
	got, err := NormalizeEnrollmentHostnames([]string{
		"Example.COM.",
		"2001:0DB8:0:0:0:0:0:1",
		"192.0.2.1",
		"example.com",
		"192.0.2.1",
	})
	if err != nil {
		t.Fatalf("normalize enrollment hostnames: %v", err)
	}
	want := []string{"192.0.2.1", "2001:db8::1", "example.com"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("normalized=%v, want %v", got, want)
	}
}

func TestNormalizeEnrollmentHostnamesRejectsNonExactValues(t *testing.T) {
	for _, value := range []string{
		"*.example.com",
		"https://example.com",
		"example.com:3443",
		"192.0.2.0/24",
		"user@example.com",
		"example.com/path",
		"example.com?query",
		"example.com#fragment",
		"[2001:db8::1]",
		"example.com..",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := NormalizeEnrollmentHostnames([]string{value}); err == nil {
				t.Fatalf("accepted non-exact enrollment hostname %q", value)
			}
		})
	}
}

func TestNormalizeEnrollmentHostnamesEnforcesBounds(t *testing.T) {
	tooMany := make([]string, MaxEnrollmentHostnames+1)
	for i := range tooMany {
		tooMany[i] = "host" + string(rune('a'+i%26)) + ".example.com"
	}
	if _, err := NormalizeEnrollmentHostnames(tooMany); err == nil {
		t.Fatal("accepted too many enrollment hostnames")
	}

	tooLong := strings.Repeat("a", MaxEnrollmentHostnameLength+1)
	if _, err := NormalizeEnrollmentHostnames([]string{tooLong}); err == nil {
		t.Fatal("accepted overlong enrollment hostname")
	}
}

func TestEnrollmentHostnamesPersistAcrossStoreViews(t *testing.T) {
	first, reopen := newEnrollmentStoreView(t)
	want := []string{"192.0.2.1", "2001:db8::1", "enroll.example.com"}
	if _, err := first.UpdateEnrollmentHostnames(t.Context(), []string{
		"Enroll.Example.COM.",
		"2001:0DB8:0:0:0:0:0:1",
		"192.0.2.1",
	}); err != nil {
		t.Fatalf("persist enrollment hostnames: %v", err)
	}

	second := reopen()
	got, err := second.GetEnrollmentHostnames(t.Context())
	if err != nil {
		t.Fatalf("read enrollment hostnames from reopened store: %v", err)
	}
	if !got.Configured || strings.Join(got.Hostnames, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("reopened enrollment hostnames=%+v, want configured %v", got, want)
	}
}

func newEnrollmentStoreView(t *testing.T) (*Store, func() *Store) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping database-backed enrollment persistence test in short mode")
	}
	dsn := os.Getenv("DATABASE_URL")
	if strings.TrimSpace(dsn) == "" {
		t.Skip("DATABASE_URL is not set")
	}

	ctx := t.Context()
	adminPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect admin db: %v", err)
	}
	t.Cleanup(adminPool.Close)
	name := regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(strings.ToLower(t.Name()), "_")
	if len(name) > 32 {
		name = name[:32]
	}
	schema := "pc_enr_" + name + "_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := adminPool.Exec(ctx, "create schema "+(pgx.Identifier{schema}).Sanitize()); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = adminPool.Exec(t.Context(), "drop schema if exists "+(pgx.Identifier{schema}).Sanitize()+" cascade")
	})

	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse db config: %v", err)
	}
	if poolCfg.ConnConfig.RuntimeParams == nil {
		poolCfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatalf("connect schema db: %v", err)
	}
	closed := false
	t.Cleanup(pool.Close)
	if err := auth.NewPostgresStore(pool).EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure auth schema: %v", err)
	}
	if err := EnsureSchema(ctx, pool); err != nil {
		t.Fatalf("ensure configuration schema: %v", err)
	}

	reopen := func() *Store {
		if !closed {
			pool.Close()
			closed = true
		}
		reopenedCfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			t.Fatalf("parse reopened db config: %v", err)
		}
		if reopenedCfg.ConnConfig.RuntimeParams == nil {
			reopenedCfg.ConnConfig.RuntimeParams = map[string]string{}
		}
		reopenedCfg.ConnConfig.RuntimeParams["search_path"] = schema
		reopened, err := pgxpool.NewWithConfig(ctx, reopenedCfg)
		if err != nil {
			t.Fatalf("open reopened db pool: %v", err)
		}
		t.Cleanup(reopened.Close)
		return New(reopened, "", domain.Ingress{})
	}
	return New(pool, "", domain.Ingress{}), reopen
}
