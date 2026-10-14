package enrollment

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	syncapply "github.com/zerkc/ProxyCore/apps/api/internal/sync"
)

func TestNodeConverterResyncImportsAndAppliesWithoutChangingIdentity(t *testing.T) {
	key := uuid.New()
	archive, enqueuer := &converterArchiveFake{}, &converterEnqueuerFake{}
	service := newConverterIdentityServiceWithKey(t, domain.TopologyRoleNode, &key)
	waiter := &converterWaiterFake{statuses: []syncapply.ApplyJobTerminal{{Status: syncapply.ApplyTerminalStatusApplied}}}
	converter := newTestNodeConverter(archive, enqueuer, service, waiter)
	before := service.Current()

	result, err := converter.Resync(context.Background(), NodeResyncInput{
		Envelope: converterEnvelope(t), LocalNodeID: before.NodeID, LocalInstallId: before.InstallationID,
		LocalIngress: domain.Ingress{IPv4: "198.51.100.20"}, ApplyWaitTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("Resync: %v", err)
	}
	if result.ApplyJobID == uuid.Nil || len(enqueuer.jobs) != 1 || len(archive.archives) != 1 {
		t.Fatalf("result=%+v jobs=%d archives=%d", result, len(enqueuer.jobs), len(archive.archives))
	}
	if archive.archives[0].Reason != "node-sync" {
		t.Fatalf("archive reason = %q", archive.archives[0].Reason)
	}
	after := service.Current()
	if after.Role != domain.TopologyRoleNode || after.LeadershipGeneration != before.LeadershipGeneration ||
		after.ClusterKeyID == nil || *after.ClusterKeyID != key {
		t.Fatalf("identity changed: before=%+v after=%+v", before, after)
	}
}

func TestNodeConverterResyncRefusesNonNodeBeforeImport(t *testing.T) {
	for _, role := range []domain.TopologyRole{domain.TopologyRoleStandalone, domain.TopologyRolePrimary} {
		archive, enqueuer := &converterArchiveFake{}, &converterEnqueuerFake{}
		service := newConverterIdentityService(t, role)
		converter := newTestNodeConverter(archive, enqueuer, service, &converterWaiterFake{})
		_, err := converter.Resync(context.Background(), NodeResyncInput{
			Envelope: converterEnvelope(t), LocalNodeID: service.Current().NodeID, LocalInstallId: service.Current().InstallationID,
		})
		if !errors.Is(err, ErrNodeConversionDenied) || len(enqueuer.jobs) != 0 || len(archive.archives) != 0 {
			t.Fatalf("role=%s err=%v jobs=%d archives=%d", role, err, len(enqueuer.jobs), len(archive.archives))
		}
	}
}

func TestNodeConverterResyncReportsFailedApply(t *testing.T) {
	key := uuid.New()
	service := newConverterIdentityServiceWithKey(t, domain.TopologyRoleNode, &key)
	waiter := &converterWaiterFake{statuses: []syncapply.ApplyJobTerminal{{Status: "failed"}}}
	converter := newTestNodeConverter(&converterArchiveFake{}, &converterEnqueuerFake{}, service, waiter)
	_, err := converter.Resync(context.Background(), NodeResyncInput{
		Envelope: converterEnvelope(t), LocalNodeID: service.Current().NodeID, LocalInstallId: service.Current().InstallationID,
		ApplyWaitTimeout: time.Second,
	})
	if !errors.Is(err, ErrNodeConversionAborted) {
		t.Fatalf("Resync = %v, want ErrNodeConversionAborted", err)
	}
}
