package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
)

func okReset(_ context.Context, username string) (string, error) {
	return "one-time-secret", nil
}

func failReset(_ context.Context, _ string) (string, error) {
	return "", errors.New("user not found")
}

func TestDispatchResetPasswordSuccess(t *testing.T) {
	var stdout, stderr bytes.Buffer
	called := ""
	reset := func(_ context.Context, username string) (string, error) {
		called = username
		return "one-time-secret", nil
	}
	code := dispatch(context.Background(), []string{"reset-password", " Owner "}, &stdout, &stderr, reset, nil)
	if code != 0 {
		t.Fatalf("expected code 0, got %d", code)
	}
	if called != " Owner " {
		t.Fatalf("expected username passed through, got %q", called)
	}
	if stdout.String() != "one-time-secret\n" {
		t.Fatalf("unexpected stdout: %q", stdout.String())
	}
	if strings.Contains(stderr.String(), "one-time-secret") {
		t.Fatal("secret leaked to stderr")
	}
}

func TestDispatchResetPasswordUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := dispatch(context.Background(), []string{"reset-password"}, &stdout, &stderr, okReset, nil)
	if code != 2 {
		t.Fatalf("expected usage code 2, got %d", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("usage error must not emit stdout, got %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Fatalf("expected usage on stderr, got %q", stderr.String())
	}
}

func TestDispatchResetPasswordRuntimeError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := dispatch(context.Background(), []string{"reset-password", "missing"}, &stdout, &stderr, failReset, nil)
	if code != 1 {
		t.Fatalf("expected runtime code 1, got %d", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("runtime error must not emit stdout, got %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "user not found") {
		t.Fatalf("expected error message, got %q", stderr.String())
	}
}

func TestDispatchNoArgsShowsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := dispatch(context.Background(), nil, &stdout, &stderr, nil, nil)
	if code != 2 {
		t.Fatalf("expected usage code 2, got %d", code)
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Fatalf("expected usage on stderr, got %q", stderr.String())
	}
}

func TestDispatchUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := dispatch(context.Background(), []string{"unknown"}, &stdout, &stderr, nil, nil)
	if code != 2 {
		t.Fatalf("expected usage code 2, got %d", code)
	}
	if !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("expected 'unknown command' on stderr, got %q", stderr.String())
	}
}

func TestDispatchSnapshotStatusSuccess(t *testing.T) {
	id := identity.Identity{
		InstallationID:        domain.InstallationID(uuid.New().String()),
		NodeID:                domain.NodeID(uuid.New().String()),
		Role:                  domain.TopologyRolePrimary,
		LeadershipGeneration:  5,
		LatestKnownGeneration: 5,
		UpdatedAt:             time.Date(2026, 5, 10, 11, 12, 13, 0, time.UTC),
	}
	status := func(_ context.Context) (identity.Identity, error) { return id, nil }
	var stdout, stderr bytes.Buffer
	code := dispatch(context.Background(), []string{"snapshot", "status"}, &stdout, &stderr, nil, status)
	if code != 0 {
		t.Fatalf("expected code 0, got %d (stderr=%q)", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"installation_id",
		"node_id",
		"role",
		"leadership_generation",
		"latest_known_generation",
		"stale_primary",
		"writable",
		"updated_at",
		string(id.InstallationID),
		string(id.NodeID),
		string(domain.TopologyRolePrimary),
		"5",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}
}

func TestDispatchSnapshotStatusNotBootstrapped(t *testing.T) {
	status := func(_ context.Context) (identity.Identity, error) { return identity.Identity{}, identity.ErrNotFound }
	var stdout, stderr bytes.Buffer
	code := dispatch(context.Background(), []string{"snapshot", "status"}, &stdout, &stderr, nil, status)
	if code != 1 {
		t.Fatalf("expected runtime code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "not bootstrapped") {
		t.Fatalf("expected 'not bootstrapped' on stderr, got %q", stderr.String())
	}
}

func TestDispatchSnapshotStatusRuntimeError(t *testing.T) {
	status := func(_ context.Context) (identity.Identity, error) {
		return identity.Identity{}, errors.New("database down")
	}
	var stdout, stderr bytes.Buffer
	code := dispatch(context.Background(), []string{"snapshot", "status"}, &stdout, &stderr, nil, status)
	if code != 1 {
		t.Fatalf("expected runtime code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "database down") {
		t.Fatalf("expected error on stderr, got %q", stderr.String())
	}
}

func TestDispatchSnapshotUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := dispatch(context.Background(), []string{"snapshot"}, &stdout, &stderr, nil, nil)
	if code != 2 {
		t.Fatalf("expected usage code 2, got %d", code)
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Fatalf("expected usage on stderr, got %q", stderr.String())
	}
}

func TestDispatchSnapshotUnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := dispatch(context.Background(), []string{"snapshot", "bogus"}, &stdout, &stderr, nil, nil)
	if code != 2 {
		t.Fatalf("expected usage code 2, got %d", code)
	}
	if !strings.Contains(stderr.String(), "unknown snapshot command") {
		t.Fatalf("expected 'unknown snapshot command' on stderr, got %q", stderr.String())
	}
}
