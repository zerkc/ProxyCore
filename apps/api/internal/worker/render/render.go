// Package render ports apps/worker/src/render.ts: it turns a job's desired
// snapshot into the CoreDNS and nginx candidates the orchestrator applies,
// using internal/renderers and internal/worker/materialize.
package render

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"strings"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/renderers"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/apply"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/control"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/materialize"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/store"
)

// Options configures candidate rendering.
type Options struct {
	// CandidateRoot defaults to apply.DefaultCandidateRoot.
	CandidateRoot string
	Capabilities  renderers.Http3Capabilities
	// Secrets resolves Basic Auth hashes and TLS keys. Without it, rendering
	// fails closed when any record needs one.
	Secrets      materialize.SecretReader
	ACMEUpstream string
}

var revisionIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// Candidates renders the job's target: coredns, nginx, or combined (coredns
// first, then nginx). Any other target, including certificate, is unsupported.
// The snapshot is the desired configuration (see apply.DesiredSnapshot).
func Candidates(ctx context.Context, snapshot json.RawMessage, job store.Job, options Options) ([]apply.Candidate, error) {
	var desired domain.ConfigurationSnapshot
	if err := json.Unmarshal(snapshot, &desired); err != nil {
		return nil, fmt.Errorf("Configuration snapshot is invalid: %w", err)
	}
	settings := desired.Settings
	if settings.DefaultPool == nil {
		return nil, errors.New("Configuration snapshot has no default resolver pool")
	}
	var zones []domain.ZoneState
	var records []domain.DNSRecord
	for _, zone := range desired.Zones {
		if zone.Enabled {
			zones = append(zones, zone)
			records = append(records, zone.Records...)
		}
	}
	root := options.CandidateRoot
	if root == "" {
		root = apply.DefaultCandidateRoot
	}
	r := renderer{ctx: ctx, desired: desired, zones: zones, records: records, root: root, job: job, options: options}
	switch job.Target {
	case store.TargetCoreDNS:
		candidate, err := r.coredns()
		return []apply.Candidate{candidate}, err
	case store.TargetNginx:
		candidate, err := r.nginx()
		return []apply.Candidate{candidate}, err
	case store.TargetCombined:
		coredns, err := r.coredns()
		if err != nil {
			return nil, err
		}
		nginx, err := r.nginx()
		if err != nil {
			return nil, err
		}
		return []apply.Candidate{coredns, nginx}, nil
	}
	return nil, fmt.Errorf("Unsupported apply target: %s", job.Target)
}

// ForJob adapts Options to the orchestrator's Renderer.
func ForJob(options Options) apply.Renderer {
	return func(ctx context.Context, snapshot json.RawMessage, job store.Job) ([]apply.Candidate, error) {
		return Candidates(ctx, snapshot, job, options)
	}
}

type renderer struct {
	ctx     context.Context
	desired domain.ConfigurationSnapshot
	zones   []domain.ZoneState
	records []domain.DNSRecord
	root    string
	job     store.Job
	options Options
}

func (r renderer) coredns() (apply.Candidate, error) {
	inputs := make([]renderers.CoreDNSZoneInput, len(r.zones))
	for i, zone := range r.zones {
		inputs[i] = renderers.CoreDNSZoneInput{Name: zone.Name, Records: zone.Records}
	}
	rendered, err := renderers.RenderCoreDNSCandidate(renderers.CoreDNSRenderInput{
		Zones:           inputs,
		Ingress:         r.desired.Settings.Ingress,
		DefaultPool:     *r.desired.Settings.DefaultPool,
		ForwardingRules: r.desired.Settings.ForwardingRules,
	})
	if err != nil {
		return apply.Candidate{}, err
	}
	path, err := r.candidatePath("coredns")
	if err != nil {
		return apply.Candidate{}, err
	}
	files := map[string]string{"Corefile": rendered.Corefile}
	maps.Copy(files, rendered.Files)
	return apply.Candidate{Service: control.ServiceCoreDNS, CandidatePath: path, Checksum: rendered.Checksum, Files: files}, nil
}

func (r renderer) nginx() (apply.Candidate, error) {
	path, err := r.candidatePath("nginx")
	if err != nil {
		return apply.Candidate{}, err
	}
	secretFiles, err := r.secretFiles()
	if err != nil {
		return apply.Candidate{}, err
	}
	rendered, err := renderers.RenderNginxCandidate(renderers.NginxRenderInput{
		Records:       r.records,
		Streams:       r.desired.Streams,
		Capabilities:  r.options.Capabilities,
		CandidatePath: path,
		ACMEUpstream:  r.options.ACMEUpstream,
		ExtraFiles:    secretFiles,
	})
	if err != nil {
		return apply.Candidate{}, err
	}
	return apply.Candidate{Service: control.ServiceNginx, CandidatePath: path, Checksum: rendered.Checksum, Files: rendered.Files}, nil
}

// secretFiles materializes Basic Auth and TLS files, or fails closed when the
// records need secrets and no store is configured.
func (r renderer) secretFiles() (map[string]string, error) {
	if r.options.Secrets == nil {
		for _, record := range r.records {
			if truthy(record.Proxy["basicAuth"]) {
				return nil, errors.New("Basic Auth secrets require a configured secret store")
			}
		}
		for _, record := range r.records {
			if record.Proxied && truthy(record.Proxy["tlsEnabled"]) {
				return nil, errors.New("TLS certificates require a configured secret store")
			}
		}
		return map[string]string{}, nil
	}
	files, err := materialize.BasicAuthFiles(r.ctx, r.records, r.options.Secrets)
	if err != nil {
		return nil, err
	}
	certificates, err := materialize.CertificateFiles(r.ctx, r.records, r.desired.Certificates, r.options.Secrets)
	if err != nil {
		return nil, err
	}
	maps.Copy(files, certificates)
	return files, nil
}

func (r renderer) candidatePath(service string) (string, error) {
	if !revisionIDPattern.MatchString(r.job.RevisionID) {
		return "", errors.New("Revision ID is invalid for a candidate path")
	}
	return strings.TrimRight(r.root, "/") + "/" + r.job.RevisionID + "/" + service, nil
}

// truthy follows JavaScript truthiness for the decoded JSON values a proxy
// setting can hold.
func truthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case float64:
		return v != 0
	}
	return true
}
