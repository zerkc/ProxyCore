// Command worker is the Go port of the Bun worker (apps/worker/src/index.ts):
// it waits for the API to migrate the SQLite database, then claims apply jobs,
// drives them through the control socket and reconciles nginx. It is built and
// published but not yet deployed by compose.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := run(ctx, os.LookupEnv, os.Stdout, os.Stderr, runOptions{})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
