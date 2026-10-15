package main

import (
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
	syncpublication "github.com/zerkc/ProxyCore/apps/api/internal/sync"
)

const (
	// nodeSyncIntervalEnv overrides how often a NODE polls its primary.
	nodeSyncIntervalEnv     = "PROXYCORE_NODE_SYNC_INTERVAL"
	minNodeSyncInterval     = 5 * time.Second
	nodeSyncApplyWaitBudget = 5 * time.Minute
)

var errNodeSyncSnapshotDenied = errors.New("primary snapshot denied")

// nodeSyncIntervalFromEnv follows the config package's envDuration convention
// (unset, unparsable or non-positive fall back to the default) and clamps to a
// floor so a typo cannot turn the primary into a hot loop. The config package
// is deliberately not touched: the interval is runtime wiring of this binary.
func nodeSyncIntervalFromEnv() time.Duration {
	value := strings.TrimSpace(os.Getenv(nodeSyncIntervalEnv))
	if value == "" {
		return syncpublication.DefaultNodeSyncInterval
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return syncpublication.DefaultNodeSyncInterval
	}
	if parsed < minNodeSyncInterval {
		return minNodeSyncInterval
	}
	return parsed
}

type nodeSyncSnapshotClient interface {
	FetchSnapshotIfChanged(ctx context.Context, primaryURL string, credential []byte, after string) (syncpublication.SnapshotFetchResult, error)
	AcknowledgeSnapshot(ctx context.Context, primaryURL string, credential []byte, nodeID, contentHash string, appliedAt time.Time) error
}

type nodeSyncResyncer interface {
	Resync(context.Context, enrollment.NodeResyncInput) (enrollment.NodeResyncResult, error)
}

type nodeSyncDependencies struct {
	Store          configuration.NodeSyncStore
	Identity       *identity.Service
	Client         nodeSyncSnapshotClient
	Converter      nodeSyncResyncer
	Validator      httpserver.EnrollmentSnapshotValidator
	LoadCredential func(ctx context.Context, credentialID string) ([]byte, error)
	Ingress        func() domain.Ingress
	Interval       time.Duration
	Logger         *log.Logger
}

// newNodeSyncer adapts the production seams to the sync loop. Credentials are
// read from the encrypted store per call, used for one request and zeroed.
func newNodeSyncer(deps nodeSyncDependencies) *syncpublication.NodeSyncer {
	if deps.Store == nil || deps.Identity == nil || deps.Client == nil || deps.Converter == nil ||
		deps.Validator == nil || deps.LoadCredential == nil {
		return nil
	}
	logf := func(string, ...any) {}
	if deps.Logger != nil {
		logf = deps.Logger.Printf
	}
	withBearer := func(ctx context.Context, credentialID string, use func(bearer []byte) error) error {
		secret, err := deps.LoadCredential(ctx, credentialID)
		if err != nil {
			// No usable local credential: the node cannot authenticate and the
			// only remedy is a fresh enrollment.
			return syncpublication.ErrSnapshotClientUnauthorized
		}
		defer clear(secret)
		credential, err := syncpublication.NewNodeCredential(credentialID, secret)
		if err != nil {
			return syncpublication.ErrSnapshotClientUnauthorized
		}
		defer credential.Destroy()
		bearer := []byte(credential.BearerCopy())
		defer clear(bearer)
		return use(bearer)
	}
	return syncpublication.NewNodeSyncer(syncpublication.NodeSyncOptions{
		Store:    deps.Store,
		Interval: deps.Interval,
		Role:     func() domain.TopologyRole { return deps.Identity.Current().Role },
		Logf:     logf,
		// Mirror the persisted unlinked state into the live identity so an
		// unlinked NODE may re-enroll; only ever set true (the converter
		// clears it once the re-enrollment commits).
		OnUnlinked: func(unlinked bool) {
			if unlinked {
				deps.Identity.SetNodeUnlinked(true)
			}
		},
		Fetch: func(ctx context.Context, state configuration.NodeSyncState, after string) (syncpublication.SnapshotFetchResult, error) {
			var result syncpublication.SnapshotFetchResult
			err := withBearer(ctx, state.CredentialID, func(bearer []byte) error {
				var fetchErr error
				result, fetchErr = deps.Client.FetchSnapshotIfChanged(ctx, state.PrimaryURL, bearer, after)
				return fetchErr
			})
			return result, err
		},
		Apply: func(ctx context.Context, envelope *replicationsnapshot.Envelope) error {
			if envelope == nil {
				return errNodeSyncSnapshotDenied
			}
			// Same gate as enrollment confirm: the node's own KEK must open
			// every replicated secret before anything is written.
			if result := deps.Validator.Validate(ctx, *envelope); !result.OK || len(result.Issues) != 0 {
				return errNodeSyncSnapshotDenied
			}
			current := deps.Identity.Current()
			var ingress domain.Ingress
			if deps.Ingress != nil {
				ingress = deps.Ingress()
			}
			_, err := deps.Converter.Resync(ctx, enrollment.NodeResyncInput{
				Envelope: envelope, LocalNodeID: current.NodeID, LocalInstallId: current.InstallationID,
				LocalIngress: ingress, ApplyWaitTimeout: nodeSyncApplyWaitBudget,
			})
			return err
		},
		Acknowledge: func(ctx context.Context, state configuration.NodeSyncState, contentHash string, appliedAt time.Time) error {
			nodeID := string(deps.Identity.Current().NodeID)
			return withBearer(ctx, state.CredentialID, func(bearer []byte) error {
				return deps.Client.AcknowledgeSnapshot(ctx, state.PrimaryURL, bearer, nodeID, contentHash, appliedAt)
			})
		},
	})
}
