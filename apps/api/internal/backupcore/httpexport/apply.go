package httpexport

import (
	"context"
	"errors"

	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/dbimport"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
)

// configStoreCreateApplyJobAdapter turns the configuration store's enqueue
// operation into the import engine's narrow post-restore apply port.
type configStoreCreateApplyJobAdapter struct {
	store configuration.ApplyJobEnqueuer
}

var _ dbimport.ApplyTrigger = (*configStoreCreateApplyJobAdapter)(nil)

func newConfigStoreCreateApplyJobAdapter(store configuration.ApplyJobEnqueuer) *configStoreCreateApplyJobAdapter {
	// A typed-nil *configuration.Store must keep meaning "nil store".
	if concrete, ok := store.(*configuration.Store); ok && concrete == nil {
		store = nil
	}
	return &configStoreCreateApplyJobAdapter{store: store}
}

// NewApplyTrigger constructs the production apply trigger used by the backup importers.
func NewApplyTrigger(store configuration.ApplyJobEnqueuer) dbimport.ApplyTrigger {
	return newConfigStoreCreateApplyJobAdapter(store)
}

func (a *configStoreCreateApplyJobAdapter) Trigger(ctx context.Context, actorID string) error {
	if a == nil || a.store == nil {
		return errors.New("httpexport: nil configuration store")
	}
	_, err := a.store.CreateApplyJob(ctx, actorID)
	return err
}
