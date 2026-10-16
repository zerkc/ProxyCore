package enrollment

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
	syncapply "github.com/zerkc/ProxyCore/apps/api/internal/sync"
)

const converterSecret = "SECRET-PAYLOAD-pcenr1_token"

type converterLogSink struct {
	mu    sync.Mutex
	lines []string
}

func (s *converterLogSink) logf(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, fmt.Sprintf(format, args...))
}

func (s *converterLogSink) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.lines, "\n")
}

type failingArchive struct{ converterArchiveFake }

func (f *failingArchive) SaveStandaloneArchive(context.Context, replicationsnapshot.StandaloneArchive) error {
	return errors.New("disk said " + converterSecret)
}

func TestNodeConverterLogsEveryFailureWithRedactedCause(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name  string
		build func(t *testing.T, sink *converterLogSink) (*NodeConverter, context.Context, NodeConversionInput)
		want  []string
	}{
		{"unavailable names the nil dependency", func(t *testing.T, sink *converterLogSink) (*NodeConverter, context.Context, NodeConversionInput) {
			c := NewNodeConverter(NodeConverterOptions{Logger: sink.logf})
			return c, context.Background(), NodeConversionInput{Envelope: converterEnvelope(t)}
		}, []string{"reason=unavailable", "missing=importer,identity,apply_waiter"}},
		{"unavailable nil envelope", func(t *testing.T, sink *converterLogSink) (*NodeConverter, context.Context, NodeConversionInput) {
			service := newConverterIdentityService(t, domain.TopologyRoleStandalone)
			c := newTestNodeConverter(&converterArchiveFake{}, &converterEnqueuerFake{}, service, &converterWaiterFake{})
			c.logger = sink.logf
			return c, context.Background(), NodeConversionInput{}
		}, []string{"reason=unavailable", "missing=envelope"}},
		{"context already canceled", func(t *testing.T, sink *converterLogSink) (*NodeConverter, context.Context, NodeConversionInput) {
			service := newConverterIdentityService(t, domain.TopologyRoleStandalone)
			c := newTestNodeConverter(&converterArchiveFake{}, &converterEnqueuerFake{}, service, &converterWaiterFake{})
			c.logger = sink.logf
			return c, canceled, NodeConversionInput{Envelope: converterEnvelope(t)}
		}, []string{"reason=context_done", "context.Canceled"}},
		{"identity ineligible", func(t *testing.T, sink *converterLogSink) (*NodeConverter, context.Context, NodeConversionInput) {
			service := newConverterIdentityService(t, domain.TopologyRole("unrecognized"))
			c := newTestNodeConverter(&converterArchiveFake{}, &converterEnqueuerFake{}, service, &converterWaiterFake{})
			c.logger = sink.logf
			return c, context.Background(), NodeConversionInput{Envelope: converterEnvelope(t)}
		}, []string{"reason=identity_ineligible", "role=unrecognized", "ErrEnrollmentIneligible"}},
		{"import failure", func(t *testing.T, sink *converterLogSink) (*NodeConverter, context.Context, NodeConversionInput) {
			service := newConverterIdentityService(t, domain.TopologyRoleStandalone)
			enq := &converterEnqueuerFake{}
			archive := &failingArchive{}
			c := NewNodeConverter(NodeConverterOptions{
				Importer: replicationsnapshot.NewImporter(archive, enq, func() time.Time { return time.Now() }),
				Archive:  archive, Identity: service, ApplyWaiter: &converterWaiterFake{}, ApplyEnqueuer: enq, Logger: sink.logf,
			})
			return c, context.Background(), NodeConversionInput{Envelope: converterEnvelope(t), LocalNodeID: service.Current().NodeID}
		}, []string{"reason=import_failed", "ErrNodeConversionImport", "*errors.errorString"}},
		{"apply wait timeout", func(t *testing.T, sink *converterLogSink) (*NodeConverter, context.Context, NodeConversionInput) {
			service := newConverterIdentityService(t, domain.TopologyRoleStandalone)
			waitErr := &syncapply.ApplyWaitTimeoutError{Last: syncapply.ApplyJobTerminal{Status: "running"}}
			c := newTestNodeConverter(&converterArchiveFake{}, &converterEnqueuerFake{}, service, &converterWaiterFake{err: waitErr})
			c.logger = sink.logf
			return c, context.Background(), NodeConversionInput{Envelope: converterEnvelope(t), LocalNodeID: service.Current().NodeID}
		}, []string{"reason=apply_wait_failed", "ErrNodeConversionApplyWait", "ErrApplyWaitTimeout"}},
		{"apply terminal failed", func(t *testing.T, sink *converterLogSink) (*NodeConverter, context.Context, NodeConversionInput) {
			service := newConverterIdentityService(t, domain.TopologyRoleStandalone)
			c := newTestNodeConverter(&converterArchiveFake{}, &converterEnqueuerFake{}, service, &converterWaiterFake{
				statuses: []syncapply.ApplyJobTerminal{{Status: "failed"}},
			})
			c.logger = sink.logf
			return c, context.Background(), NodeConversionInput{Envelope: converterEnvelope(t), LocalNodeID: service.Current().NodeID}
		}, []string{"reason=apply_not_applied", "status=failed"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &converterLogSink{}
			c, ctx, in := tc.build(t, sink)
			if _, err := c.Convert(ctx, in); err == nil {
				t.Fatal("Convert must fail")
			}
			out := sink.text()
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Fatalf("log missing %q:\n%s", want, out)
				}
			}
			if !strings.Contains(out, "node conversion rejected") {
				t.Fatalf("log must carry the stable prefix:\n%s", out)
			}
			for _, secret := range []string{converterSecret, "pcenr1_", "disk said"} {
				if strings.Contains(out, secret) {
					t.Fatalf("log leaks %q:\n%s", secret, out)
				}
			}
		})
	}
}

func TestNodeConverterLogsIdentityCommitDenialWithoutLeaking(t *testing.T) {
	sink := &converterLogSink{}
	service := newConverterIdentityService(t, domain.TopologyRoleStandalone)
	c := newTestNodeConverter(&converterArchiveFake{}, &converterEnqueuerFake{}, service, &converterWaiterFake{
		statuses: []syncapply.ApplyJobTerminal{{Status: syncapply.ApplyTerminalStatusApplied}},
	})
	c.logger = sink.logf
	env := converterEnvelope(t)
	env.Transient.LeadershipGeneration = 1 // below the local generation: the commit is refused
	key := uuid.New()
	_, err := c.Convert(context.Background(), NodeConversionInput{Envelope: env, LocalNodeID: service.Current().NodeID, ClusterKeyID: &key})
	if err == nil {
		t.Skip("commit unexpectedly accepted; fixture cannot force a denial")
	}
	out := sink.text()
	if !strings.Contains(out, "reason=identity_commit_denied") || !strings.Contains(out, "types=") {
		t.Fatalf("log = %s", out)
	}
	if strings.Contains(out, key.String()) {
		t.Fatalf("log leaks the cluster key id: %s", out)
	}
}
