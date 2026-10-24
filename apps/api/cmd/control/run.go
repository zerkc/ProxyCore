package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/worker/controlserver"
)

const defaultSocketPath = "/run/proxycore/control.sock"

type fixedBackend struct{}

func (fixedBackend) Handlers() controlserver.Handlers { return controlserver.FixedBoundaryHandlers() }

// lineLogger writes "<ISO> <message>\n" lines like the TS stderr logger.
func lineLogger(w io.Writer) func(string) {
	var mu sync.Mutex
	return func(message string) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(w, "%s %s\n", time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), message)
	}
}

// socketPath mirrors `WORKER_SOCKET_PATH ?? default`: only an unset variable
// selects the default.
func socketPath(lookup func(string) (string, bool)) string {
	if value, ok := lookup("WORKER_SOCKET_PATH"); ok {
		return value
	}
	return defaultSocketPath
}

// selectBackend picks the handler table. Anything but "docker" uses the fixed
// fallback, as index.ts does; docker fails fast until the T4b backend exists.
func selectBackend(lookup func(string) (string, bool)) (controlserver.Backend, error) {
	if backend, _ := lookup("CONTROL_BACKEND"); backend == "docker" {
		return nil, errors.New("CONTROL_BACKEND=docker: the Go control server's docker backend is not available yet")
	}
	return fixedBackend{}, nil
}

// run serves until ctx is cancelled and returns nil after a clean shutdown.
func run(ctx context.Context, lookup func(string) (string, bool), stdout, stderr io.Writer) error {
	backend, err := selectBackend(lookup)
	if err != nil {
		return err
	}
	path := socketPath(lookup)
	server, err := controlserver.Listen(path, controlserver.NewDispatcher(backend.Handlers()), lineLogger(stderr))
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "proxycore-control listening on %s\n", path)
	return server.Serve(ctx)
}
