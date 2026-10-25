package controldocker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/zerkc/ProxyCore/apps/api/internal/worker/control"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/controlserver"
)

// Containers names the container of each controlled service.
type Containers struct {
	CoreDNS string
	Nginx   string
}

// Options configures the backend. Empty strings are used as given, like the
// `??` defaults of docker-control.ts only replace undefined; use DefaultOptions
// and override the fields whose environment variable is set.
type Options struct {
	CandidateRoot string
	ZonesRoot     string
	// ConfigRoot is the persisted Corefile directory (mounted into CoreDNS as
	// /etc/coredns-config).
	ConfigRoot string
	Containers Containers
}

// DefaultOptions returns the defaults of createDockerServiceControl.
func DefaultOptions() Options {
	return Options{
		CandidateRoot: "/var/lib/proxycore/candidates",
		ZonesRoot:     "/var/lib/proxycore/coredns-zones",
		ConfigRoot:    "/var/lib/proxycore/coredns-config",
		Containers:    Containers{CoreDNS: "proxycore-coredns", Nginx: "proxycore-nginx"},
	}
}

// Backend implements controlserver.Backend over Docker.
type Backend struct {
	docker Docker
	opts   Options
}

var _ controlserver.Backend = (*Backend)(nil)

// New builds the Docker backend.
func New(docker Docker, opts Options) *Backend { return &Backend{docker: docker, opts: opts} }

// Handlers maps every operation of both services to the shared executor.
func (b *Backend) Handlers() controlserver.Handlers {
	handlers := controlserver.Handlers{}
	for _, service := range []control.Service{control.ServiceCoreDNS, control.ServiceNginx} {
		handlers[service] = map[control.Operation]controlserver.Handler{}
		for _, op := range control.Operations {
			handlers[service][op] = b.execute
		}
	}
	return handlers
}

func (b *Backend) container(service control.Service) string {
	if service == control.ServiceNginx {
		return b.opts.Containers.Nginx
	}
	return b.opts.Containers.CoreDNS
}

// AssertCandidatePath keeps candidate paths below the fixed worker root.
func AssertCandidatePath(candidatePath, candidateRoot string) error {
	if !strings.HasPrefix(candidatePath, candidateRoot+"/") ||
		strings.Contains(candidatePath, "..") ||
		strings.Contains(candidatePath, "\x00") {
		return fmt.Errorf("Candidate path is outside the fixed worker root")
	}
	return nil
}

func obj(pairs ...string) json.RawMessage {
	var b strings.Builder
	b.WriteByte('{')
	for i := 0; i < len(pairs); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(controlserver.QuoteJS(pairs[i]) + ":" + controlserver.QuoteJS(pairs[i+1]))
	}
	b.WriteByte('}')
	return json.RawMessage(b.String())
}

func (b *Backend) execute(ctx context.Context, req control.Request) (json.RawMessage, error) {
	if err := AssertCandidatePath(req.CandidatePath, b.opts.CandidateRoot); err != nil {
		return nil, err
	}
	name := b.container(req.Service)
	nginx := req.Service == control.ServiceNginx
	switch req.Operation {
	case control.OperationStage:
		if _, err := os.Stat(req.CandidatePath); err != nil {
			return nil, err
		}
		return obj("status", "staged", "candidatePath", req.CandidatePath), nil
	case control.OperationValidate:
		if nginx {
			if err := b.exec(ctx, name, "nginx", "-t", "-c", req.CandidatePath+"/nginx.conf"); err != nil {
				return nil, err
			}
		} else if _, err := os.Stat(req.CandidatePath + "/Corefile"); err != nil {
			return nil, err
		}
		return obj("status", "validated", "checksum", req.Checksum), nil
	case control.OperationPromote:
		var err error
		if nginx {
			err = b.promoteNginx(ctx, name, req.CandidatePath)
		} else {
			err = b.promoteCoreDNS(ctx, name, req.CandidatePath)
		}
		if err != nil {
			return nil, err
		}
		return obj("status", "promoted", "revisionId", req.RevisionID), nil
	case control.OperationReload:
		if nginx {
			if err := b.exec(ctx, name, "nginx", "-s", "reload"); err != nil {
				return nil, err
			}
		} else if err := b.docker.Restart(ctx, name, 10); err != nil {
			return nil, err
		}
		return obj("status", "reloaded"), nil
	case control.OperationHealth:
		running, err := b.docker.Running(ctx, name)
		if err != nil {
			return nil, err
		}
		if !running {
			return nil, fmt.Errorf("%s container is not running", req.Service)
		}
		if !nginx {
			return obj("status", "healthy"), nil
		}
		observation, err := b.probeNginxActiveConfig(ctx, name, req.CandidatePath)
		if err != nil {
			return nil, err
		}
		if observation.Reason == "" {
			return obj("status", observation.Status, "revisionId", req.RevisionID), nil
		}
		return obj("status", observation.Status, "reason", observation.Reason, "revisionId", req.RevisionID), nil
	case control.OperationRollback:
		var err error
		if nginx {
			err = b.rollbackNginx(ctx, name)
		} else {
			err = b.rollbackCoreDNS(ctx, name)
		}
		if err != nil {
			return nil, err
		}
		return obj("status", "rolled-back"), nil
	}
	return nil, fmt.Errorf("Operation %s is unavailable for %s", req.Operation, req.Service)
}

// exec runs a command and turns a non-zero exit into the fixed error text.
func (b *Backend) exec(ctx context.Context, name string, cmd ...string) error {
	code, err := b.docker.Exec(ctx, name, cmd)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("Fixed service operation failed with exit %d", code)
	}
	return nil
}

// StartupSeed restores the persisted Corefile from the newest applied candidate
// and restarts CoreDNS (t=5) to load it, mirroring the docker block of main() in
// index.ts. Messages go through stdout/stderr with their trailing newline. A
// failed restart is reported on stderr and is not an error.
func StartupSeed(ctx context.Context, docker Docker, opts Options, stdout, stderr func(string)) error {
	seeded, err := SeedLiveCorefileFromCandidates(opts.CandidateRoot, opts.ConfigRoot)
	if err != nil {
		return err
	}
	if seeded == "" {
		return nil
	}
	stdout("proxycore-control restored persisted CoreDNS Corefile from " + seeded + "\n")
	name := opts.Containers.CoreDNS
	if err := docker.Restart(ctx, name, 5); err != nil {
		stderr("proxycore-control could not restart CoreDNS after Corefile seed: " + err.Error() + "\n")
		return nil
	}
	stdout("proxycore-control restarted " + name + " to load Corefile\n")
	return nil
}
