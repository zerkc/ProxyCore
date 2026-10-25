// Command control is the Go port of the control server (apps/control/src/index.ts):
// it serves the fixed service-control operations on a Unix socket, either with the
// fixed fallback or, for CONTROL_BACKEND=docker, with the Docker backend.
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
	if err := run(ctx, os.LookupEnv, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
