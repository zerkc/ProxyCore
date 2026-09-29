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
	store *configuration.Store
}

var _ dbimport.ApplyTrigger = (*configStoreCreateApplyJobAdapter)(nil)

func newConfigStoreCreateApplyJobAdapter(store *configuration.Store) *configStoreCreateApplyJobAdapter {
	return &configStoreCreateApplyJobAdapter{store: store}
}

// NewApplyTrigger constructs the production apply trigger used by BackupImporter.
func NewApplyTrigger(store *configuration.Store) dbimport.ApplyTrigger {
	return newConfigStoreCreateApplyJobAdapter(store)
}

func (a *configStoreCreateApplyJobAdapter) Trigger(ctx context.Context, actorID string) error {
	if a == nil || a.store == nil {
		return errors.New("httpexport: nil configuration store")
	}
	_, err := a.store.CreateApplyJob(ctx, actorID)
	return err
}
