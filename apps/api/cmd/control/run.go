package main

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/worker/controldocker"
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

// envOr mirrors `process.env.NAME ?? fallback`: only an unset variable selects
// the fallback, an empty value is kept.
func envOr(lookup func(string) (string, bool), name, fallback string) string {
	if value, ok := lookup(name); ok {
		return value
	}
	return fallback
}

// dialDocker connects to the Docker daemon socket.
type dialDocker func(socketPath string) (controldocker.Docker, error)

func dialSDK(socketPath string) (controldocker.Docker, error) {
	return controldocker.NewClient(socketPath)
}

// dockerOptions reads the backend environment like index.ts.
func dockerOptions(lookup func(string) (string, bool)) controldocker.Options {
	defaults := controldocker.DefaultOptions()
	return controldocker.Options{
		CandidateRoot: envOr(lookup, "CANDIDATE_ROOT", defaults.CandidateRoot),
		ZonesRoot:     envOr(lookup, "COREDNS_ZONES_ROOT", defaults.ZonesRoot),
		ConfigRoot:    envOr(lookup, "COREDNS_CONFIG_ROOT", defaults.ConfigRoot),
		Containers: controldocker.Containers{
			CoreDNS: envOr(lookup, "COREDNS_CONTAINER_NAME", defaults.Containers.CoreDNS),
			Nginx:   envOr(lookup, "NGINX_CONTAINER_NAME", defaults.Containers.Nginx),
		},
	}
}

// selectBackend picks the handler table: the Docker backend (after restoring the
// persisted CoreDNS Corefile, as index.ts does) for CONTROL_BACKEND=docker and
// the fixed fallback for anything else.
func selectBackend(ctx context.Context, lookup func(string) (string, bool), stdout, stderr io.Writer, dial dialDocker) (controlserver.Backend, error) {
	if backend, _ := lookup("CONTROL_BACKEND"); backend != "docker" {
		return fixedBackend{}, nil
	}
	docker, err := dial(envOr(lookup, "DOCKER_SOCKET_PATH", controldocker.DefaultSocketPath))
	if err != nil {
		return nil, err
	}
	options := dockerOptions(lookup)
	write := func(w io.Writer) func(string) {
		return func(message string) { io.WriteString(w, message) }
	}
	if err := controldocker.StartupSeed(ctx, docker, options, write(stdout), write(stderr)); err != nil {
		return nil, err
	}
	return controldocker.New(docker, options), nil
}

// run serves until ctx is cancelled and returns nil after a clean shutdown.
func run(ctx context.Context, lookup func(string) (string, bool), stdout, stderr io.Writer) error {
	return runWith(ctx, lookup, stdout, stderr, dialSDK)
}

func runWith(ctx context.Context, lookup func(string) (string, bool), stdout, stderr io.Writer, dial dialDocker) error {
	backend, err := selectBackend(ctx, lookup, stdout, stderr, dial)
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
