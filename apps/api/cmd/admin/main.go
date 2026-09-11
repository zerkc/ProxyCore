package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/config"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
)

// resetFunc matches the auth.Service.ResetPassword signature so the
// dispatch can be unit-tested without spinning up a real PostgreSQL.
type resetFunc func(context.Context, string) (string, error)

// snapshotStatusFunc matches the identity.Service.Load signature so the
// dispatch can be unit-tested without a database.
type snapshotStatusFunc func(context.Context) (identity.Identity, error)

// dispatch routes admin subcommands to the matching handler. It returns
// the process exit code; 0 means success, 2 means usage error, 1 means
// the command failed at runtime.
func dispatch(ctx context.Context, args []string, stdout, stderr io.Writer, reset resetFunc, status snapshotStatusFunc) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: proxycore-admin <command> [args]")
		fmt.Fprintln(stderr, "commands:")
		fmt.Fprintln(stderr, "  reset-password <username>")
		fmt.Fprintln(stderr, "  snapshot status")
		return 2
	}
	switch args[0] {
	case "reset-password":
		return runResetPassword(ctx, args[1:], stdout, stderr, reset)
	case "snapshot":
		return runSnapshot(ctx, args[1:], stdout, stderr, status)
	default:
		fmt.Fprintf(stderr, "unknown command: %s\n", args[0])
		return 2
	}
}

func runResetPassword(ctx context.Context, args []string, stdout, stderr io.Writer, reset resetFunc) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: proxycore-admin reset-password <username>")
		return 2
	}
	password, err := reset(ctx, args[0])
	if err != nil {
		fmt.Fprintf(stderr, "reset password: %v\n", err)
		return 1
	}
	// This is the only emission of the temporary password.
	fmt.Fprintln(stdout, password)
	return 0
}

func runSnapshot(ctx context.Context, args []string, stdout, stderr io.Writer, status snapshotStatusFunc) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: proxycore-admin snapshot <command> [args]")
		fmt.Fprintln(stderr, "commands:")
		fmt.Fprintln(stderr, "  status")
		return 2
	}
	switch args[0] {
	case "status":
		return runSnapshotStatus(ctx, args[1:], stdout, stderr, status)
	default:
		fmt.Fprintf(stderr, "unknown snapshot command: %s\n", args[0])
		return 2
	}
}

func runSnapshotStatus(ctx context.Context, args []string, stdout, stderr io.Writer, status snapshotStatusFunc) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "usage: proxycore-admin snapshot status")
		return 2
	}
	id, err := status(ctx)
	if err != nil {
		if errors.Is(err, identity.ErrNotFound) {
			fmt.Fprintln(stderr, "snapshot status: installation identity is not bootstrapped")
			return 1
		}
		fmt.Fprintf(stderr, "snapshot status: %v\n", err)
		return 1
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "FIELD\tVALUE")
	fmt.Fprintf(tw, "installation_id\t%s\n", id.InstallationID)
	fmt.Fprintf(tw, "node_id\t%s\n", id.NodeID)
	fmt.Fprintf(tw, "role\t%s\n", id.Role)
	fmt.Fprintf(tw, "leadership_generation\t%d\n", id.LeadershipGeneration)
	fmt.Fprintf(tw, "latest_known_generation\t%d\n", id.LatestKnownGeneration)
	fmt.Fprintf(tw, "stale_primary\t%t\n", id.IsStalePrimary())
	fmt.Fprintf(tw, "writable\t%t\n", id.IsWritable())
	if id.ClusterKeyID != nil {
		fmt.Fprintf(tw, "cluster_key_id\t%s\n", id.ClusterKeyID)
	}
	if !id.UpdatedAt.IsZero() {
		fmt.Fprintf(tw, "updated_at\t%s\n", id.UpdatedAt.UTC().Format(time.RFC3339))
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(stderr, "snapshot status: %v\n", err)
		return 1
	}
	return 0
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}
	if cfg.DatabaseURL == "" {
		fmt.Fprintln(os.Stderr, "config: DATABASE_URL is required")
		os.Exit(1)
	}
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "database: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	if args := os.Args[1:]; len(args) > 0 && args[0] == "reset-password" {
		store := auth.NewPostgresStore(pool)
		if err := store.EnsureSchema(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "database schema: %v\n", err)
			os.Exit(1)
		}
		svc := auth.NewService(store, auth.ServiceOptions{})
		os.Exit(runResetPassword(ctx, args[1:], os.Stdout, os.Stderr, svc.ResetPassword))
	}

	identityStore := identity.NewPgStore(pool)
	identitySvc := identity.NewService(identityStore)
	os.Exit(dispatch(ctx, os.Args[1:], os.Stdout, os.Stderr, nil, identitySvc.Load))
}

// guard against the identity import being dropped during future edits; the
// type is used transitively via identitySvc.Load.
var _ = domain.TopologyRoleStandalone
