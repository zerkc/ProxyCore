// Package controldocker is the Docker backend of the Go control server, a port
// of apps/control/src/docker-control.ts. Container access goes through the small
// Docker interface so the operations can be tested without a daemon.
package controldocker

import "context"

// Docker is the set of Docker calls the control operations use.
type Docker interface {
	// Running reports State.Running of the named container.
	Running(ctx context.Context, container string) (bool, error)
	// Restart restarts the container, waiting timeoutSeconds before a kill.
	Restart(ctx context.Context, container string, timeoutSeconds int) error
	// Exec runs cmd in the container, drains its output and returns the exit code.
	Exec(ctx context.Context, container string, cmd []string) (exitCode int, err error)
	// PutArchive extracts a tar archive into the directory dir of the container.
	PutArchive(ctx context.Context, container, dir string, archive []byte) error
	// GetArchive returns the tar stream for path inside the container.
	GetArchive(ctx context.Context, container, path string) ([]byte, error)
}
