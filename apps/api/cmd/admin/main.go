package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/config"
)

type resetFunc func(context.Context, string) (string, error)

func run(ctx context.Context, args []string, stdout, stderr io.Writer, reset resetFunc) int {
	if len(args) != 2 || args[0] != "reset-password" {
		fmt.Fprintln(stderr, "usage: proxycore-admin reset-password <username>")
		return 2
	}
	password, err := reset(ctx, args[1])
	if err != nil {
		fmt.Fprintf(stderr, "reset password: %v\n", err)
		return 1
	}
	// This is the only emission of the temporary password.
	fmt.Fprintln(stdout, password)
	return 0
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := config.Load()
	if err != nil { fmt.Fprintf(os.Stderr, "config: %v\n", err); os.Exit(1) }
	if cfg.DatabaseURL == "" { fmt.Fprintln(os.Stderr, "config: DATABASE_URL is required"); os.Exit(1) }
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil { fmt.Fprintf(os.Stderr, "database: %v\n", err); os.Exit(1) }
	defer pool.Close()
	store := auth.NewPostgresStore(pool)
	if err := store.EnsureSchema(ctx); err != nil { fmt.Fprintf(os.Stderr, "database schema: %v\n", err); os.Exit(1) }
	svc := auth.NewService(store, auth.ServiceOptions{})
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr, svc.ResetPassword))
}
