package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/config"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
)

func TestOpenAdminRuntimeRequiresSQLitePath(t *testing.T) {
	if _, err := openAdminRuntime(context.Background(), config.Config{}); !errors.Is(err, errSQLitePathRequired) {
		t.Fatalf("error = %v, want errSQLitePathRequired", err)
	}
}

func TestOpenAdminRuntimeUsesSQLite(t *testing.T) {
	cfg := config.Config{SQLitePath: filepath.Join(t.TempDir(), "proxycore.sqlite")}
	rt, err := openAdminRuntime(context.Background(), cfg)
	if err != nil {
		t.Fatalf("openAdminRuntime: %v", err)
	}
	defer rt.Close()
	if _, err := rt.status(context.Background()); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("status on empty database = %v, want identity.ErrNotFound", err)
	}
	if _, err := rt.reset(context.Background(), "ghost"); err == nil {
		t.Fatal("reset for unknown user must fail")
	}
}
