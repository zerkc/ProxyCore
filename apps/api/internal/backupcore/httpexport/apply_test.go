package httpexport

import (
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/dbimport"
)

func TestConfigStoreApplyAdapterImplementsApplyTrigger(t *testing.T) {
	var _ dbimport.ApplyTrigger = (*configStoreCreateApplyJobAdapter)(nil)
}
