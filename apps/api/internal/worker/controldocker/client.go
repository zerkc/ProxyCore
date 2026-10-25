package controldocker

import (
	"bytes"
	"context"
	"io"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// DefaultSocketPath is the Docker socket used when DOCKER_SOCKET_PATH is unset.
const DefaultSocketPath = "/var/run/docker.sock"

// Client implements Docker over the official Docker SDK.
type Client struct {
	api *client.Client
}

var _ Docker = (*Client)(nil)

// NewClient connects to the Docker daemon on a Unix socket and negotiates the
// API version on first use. It does not contact the daemon.
func NewClient(socketPath string) (*Client, error) {
	api, err := client.NewClientWithOpts(
		client.WithHost("unix://"+socketPath),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, err
	}
	return &Client{api: api}, nil
}

// Close releases the SDK client.
func (c *Client) Close() error { return c.api.Close() }

func (c *Client) Running(ctx context.Context, name string) (bool, error) {
	info, err := c.api.ContainerInspect(ctx, name)
	if err != nil {
		return false, err
	}
	return info.State != nil && info.State.Running, nil
}

func (c *Client) Restart(ctx context.Context, name string, timeoutSeconds int) error {
	return c.api.ContainerRestart(ctx, name, container.StopOptions{Timeout: &timeoutSeconds})
}

const execPollInterval = 10 * time.Millisecond

func (c *Client) Exec(ctx context.Context, name string, cmd []string) (int, error) {
	created, err := c.api.ContainerExecCreate(ctx, name, container.ExecOptions{
		Cmd: cmd, AttachStdout: true, AttachStderr: true,
	})
	if err != nil {
		return 0, err
	}
	attached, err := c.api.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		return 0, err
	}
	_, copyErr := io.Copy(io.Discard, attached.Reader)
	attached.Close()
	if copyErr != nil {
		return 0, copyErr
	}
	for {
		info, err := c.api.ContainerExecInspect(ctx, created.ID)
		if err != nil {
			return 0, err
		}
		if !info.Running {
			return info.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(execPollInterval):
		}
	}
}

// PutArchive omits noOverwriteDirNonDir (AllowOverwriteDirWithFile) so the
// request matches dockerode's putArchive, which never sets it.
func (c *Client) PutArchive(ctx context.Context, name, dir string, archive []byte) error {
	return c.api.CopyToContainer(ctx, name, dir, bytes.NewReader(archive), container.CopyToContainerOptions{AllowOverwriteDirWithFile: true})
}

func (c *Client) GetArchive(ctx context.Context, name, path string) ([]byte, error) {
	stream, _, err := c.api.CopyFromContainer(ctx, name, path)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	data, err := io.ReadAll(stream)
	if err != nil {
		return nil, err
	}
	return data, nil
}
