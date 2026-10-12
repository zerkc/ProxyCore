package httpexport

import (
	"context"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/dbimport"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
)

func TestConfigStoreApplyAdapterImplementsApplyTrigger(t *testing.T) {
	var _ dbimport.ApplyTrigger = (*configStoreCreateApplyJobAdapter)(nil)
}

type fakeApplyJobEnqueuer struct{ calls int }

func (f *fakeApplyJobEnqueuer) CreateApplyJob(_ context.Context, _ string) (configuration.ApplyResult, error) {
	f.calls++
	return configuration.ApplyResult{}, nil
}

func TestApplyTriggerUsesMinimalEnqueuerAndTypedNilIsNilStore(t *testing.T) {
	fake := &fakeApplyJobEnqueuer{}
	if err := NewApplyTrigger(fake).Trigger(context.Background(), "actor"); err != nil || fake.calls != 1 {
		t.Fatalf("Trigger err=%v calls=%d", err, fake.calls)
	}
	if err := NewApplyTrigger((*configuration.SQLiteConfigurationBackend)(nil)).Trigger(context.Background(), "actor"); err == nil {
		t.Fatal("typed-nil store must produce the nil-store error, not a panic")
	}
}
