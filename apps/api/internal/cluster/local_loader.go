package cluster

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
)

var (
	ErrLocalKEKLoaderNotReady = errors.New("local cluster KEK is not ready")
	ErrLocalKEKLoaderDenied   = errors.New("local cluster KEK access denied")
	ErrLocalKEKLoaderStore    = errors.New("local cluster KEK store unavailable")
)

// IdentitySource is the loaded identity boundary used by the local KEK
// loader. Load remains part of the source contract for startup adapters; this
// loader deliberately reads Current and never performs an implicit load.
type IdentitySource interface {
	Current() identity.Identity
	Load(context.Context) (identity.Identity, error)
}

type LocalKEKLoader struct {
	pool     *pgxpool.Pool
	store    *Store
	identity IdentitySource
	now      func() time.Time
}

type LocalKEKLoaderOptions struct {
	Pool     *pgxpool.Pool
	Store    *Store
	Identity IdentitySource
	Now      func() time.Time
}

func NewLocalKEKLoader(opts LocalKEKLoaderOptions) *LocalKEKLoader {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &LocalKEKLoader{pool: opts.Pool, store: opts.Store, identity: opts.Identity, now: now}
}

// LoadLocalKEK reads the already-loaded local identity, loads the referenced
// encrypted KEK in one owned transaction, commits that transaction, and then
// returns a caller-owned in-memory KEK. The caller must call Destroy.
func (l *LocalKEKLoader) LoadLocalKEK(ctx context.Context) (*KEK, uuid.UUID, error) {
	if l == nil || ctx == nil || l.identity == nil {
		return nil, uuid.Nil, ErrLocalKEKLoaderNotReady
	}
	current, loaded := localKEKCurrent(l.identity)
	if !loaded || !localKEKIdentityReady(current) {
		return nil, uuid.Nil, ErrLocalKEKLoaderNotReady
	}
	if current.Role != domain.TopologyRoleNode && current.Role != domain.TopologyRolePrimaryWithNodes {
		return nil, uuid.Nil, ErrLocalKEKLoaderDenied
	}
	if current.ClusterKeyID == nil {
		return nil, uuid.Nil, ErrLocalKEKLoaderNotReady
	}
	activeID := *current.ClusterKeyID
	if l.pool == nil || l.store == nil {
		return nil, uuid.Nil, ErrLocalKEKLoaderStore
	}

	tx, err := l.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, uuid.Nil, ErrLocalKEKLoaderStore
	}
	defer tx.Rollback(ctx)

	material, err := l.store.LoadOrCreate(ctx, tx, &activeID)
	if err != nil {
		material.Destroy()
		return nil, uuid.Nil, ErrLocalKEKLoaderStore
	}
	keyBytes := material.CopyBytes()
	materialID := material.ID
	material.Destroy()
	kek, err := NewKEK(keyBytes)
	zeroBytes(keyBytes)
	if err != nil || materialID != activeID {
		if kek != nil {
			kek.Destroy()
		}
		return nil, uuid.Nil, ErrLocalKEKLoaderStore
	}
	if err := tx.Commit(ctx); err != nil {
		kek.Destroy()
		return nil, uuid.Nil, ErrLocalKEKLoaderStore
	}
	return kek, materialID, nil
}

func localKEKCurrent(source IdentitySource) (current identity.Identity, loaded bool) {
	defer func() {
		if recover() != nil {
			current = identity.Identity{}
			loaded = false
		}
	}()
	current = source.Current()
	return current, true
}

func localKEKIdentityReady(current identity.Identity) bool {
	return current.Role.IsValid() && current.InstallationID.IsValid() && current.NodeID.IsValid() &&
		current.LeadershipGeneration > 0 && current.LatestKnownGeneration >= current.LeadershipGeneration
}

var _ IdentitySource = (*identity.Service)(nil)
