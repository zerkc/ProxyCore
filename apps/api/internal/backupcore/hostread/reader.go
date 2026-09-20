package hostread

import (
	"context"

	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore"
)

// NamedFile aliases the shared backup entry source type. The concrete shape is
// owned by backupcore so all backup producers can be consumed by one packager.
type NamedFile = backupcore.NamedFile

// HostSnapshotReader reads host-local files and the currently applied revision
// needed to build a disaster-recovery snapshot.
type HostSnapshotReader interface {
	EnvFile(ctx context.Context) (NamedFile, error)
	AppliedRevision(ctx context.Context) (string, error)
	AppliedCertFiles(ctx context.Context, revisionID string) ([]NamedFile, error)
}
