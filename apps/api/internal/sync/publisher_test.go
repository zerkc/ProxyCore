package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
)

func TestSnapshotPublisherPublishesAuthorizedLatestSnapshot(t *testing.T) {
	publisher, store, _, credential := newFakeSnapshotPublisher(t)
	defer credential.Destroy()
	got, err := publisher.Publish(context.Background(), SnapshotRequest{Credential: credential.BearerCopy()})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got.Metadata.ContentHash != store.publication.Snapshot.ContentHash {
		t.Fatalf("content hash = %q, want %q", got.Metadata.ContentHash, store.publication.Snapshot.ContentHash)
	}
	if string(got.Bytes) != string(store.publication.Snapshot.Bytes) {
		t.Fatalf("body = %q, want %q", got.Bytes, store.publication.Snapshot.Bytes)
	}
}

type fakeSnapshotIdentityReader struct {
	current identity.Identity
	err     error
}

func (f fakeSnapshotIdentityReader) SnapshotPublicationIdentity() (identity.Identity, error) {
	if f.err != nil {
		return identity.Identity{}, f.err
	}
	return f.current, nil
}

type fakeSnapshotPublicationStore struct {
	credential       configuration.SnapshotPublicationCredentialRecord
	publication      configuration.SnapshotPublicationResult
	err              error
	authenticateCall int
}

func (f *fakeSnapshotPublicationStore) ReadSnapshotPublication(
	_ context.Context,
	request configuration.SnapshotPublicationRequest,
) (configuration.SnapshotPublicationResult, error) {
	if f.err != nil {
		return configuration.SnapshotPublicationResult{}, f.err
	}
	if request.Authenticate == nil || request.MaxBytes <= 0 {
		return configuration.SnapshotPublicationResult{}, ErrSnapshotDenied
	}
	f.authenticateCall++
	principal, err := request.Authenticate(request.PresentedCredential, f.credential)
	if err != nil {
		return configuration.SnapshotPublicationResult{}, err
	}
	result := f.publication
	result.Principal = principal
	if len(result.Snapshot.Bytes) > request.MaxBytes {
		return configuration.SnapshotPublicationResult{}, ErrSnapshotDenied
	}
	if request.AfterContentHash != "" && request.AfterContentHash == result.Snapshot.ContentHash {
		result.Current = true
		result.Snapshot.Bytes = nil
		return result, nil
	}
	result.Snapshot.Bytes = append([]byte(nil), result.Snapshot.Bytes...)
	return result, nil
}

func TestSnapshotPublisherAuthenticatesBeforeMalformedAfter(t *testing.T) {
	for _, after := range []string{"malformed", strings.Repeat("A", SnapshotContentHashHexLength), strings.Repeat("0", SnapshotContentHashHexLength-1), strings.Repeat("g", SnapshotContentHashHexLength)} {
		t.Run(after, func(t *testing.T) {
			publisher, store, _, credential := newFakeSnapshotPublisher(t)
			defer credential.Destroy()
			if _, err := publisher.Publish(context.Background(), SnapshotRequest{Credential: credential.BearerCopy(), After: after}); err != ErrSnapshotDenied {
				t.Fatalf("malformed after error = %v, want %v", err, ErrSnapshotDenied)
			}
			if store.authenticateCall != 1 {
				t.Fatalf("authentication calls = %d, want 1 before malformed-after denial", store.authenticateCall)
			}
		})
	}
}

func TestSnapshotPublisherRejectsValidJSONWithoutCanonicalEnvelope(t *testing.T) {
	publisher, store, _, credential := newFakeSnapshotPublisher(t)
	defer credential.Destroy()
	body := []byte(`{"settings":{},"zones":[]}`)
	sum := sha256.Sum256(body)
	store.publication.Snapshot.Bytes = body
	store.publication.Snapshot.ContentHash = hex.EncodeToString(sum[:])
	if _, err := publisher.Publish(context.Background(), SnapshotRequest{Credential: credential.BearerCopy()}); err != ErrSnapshotDenied {
		t.Fatalf("raw JSON publication error = %v, want %v", err, ErrSnapshotDenied)
	}
}

func TestSnapshotPublisherRejectsMissingEnvelopeClusterKeyRef(t *testing.T) {
	publisher, store, _, credential := newFakeSnapshotPublisher(t)
	defer credential.Destroy()
	body, hash := mutatePublicationEnvelope(t, store.publication.Snapshot.Bytes, func(envelope *replicationsnapshot.Envelope) {
		envelope.NodeLocal.ClusterKeyRef = nil
	})
	store.publication.Snapshot.Bytes = body
	store.publication.Snapshot.ContentHash = hash
	if _, err := publisher.Publish(context.Background(), SnapshotRequest{Credential: credential.BearerCopy()}); err != ErrSnapshotDenied {
		t.Fatalf("missing ClusterKeyRef error = %v, want %v", err, ErrSnapshotDenied)
	}
}

func TestSnapshotPublisherRejectsEnvelopeIntegrityAndLineageFailures(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*replicationsnapshot.Envelope)
	}{
		{name: "wrong canonical hash", mutate: func(envelope *replicationsnapshot.Envelope) {
			envelope.ContentHash = strings.Repeat("0", SnapshotContentHashHexLength)
		}},
		{name: "incompatible version", mutate: func(envelope *replicationsnapshot.Envelope) {
			envelope.Transient.SnapshotVersion = 2
		}},
		{name: "source mismatch", mutate: func(envelope *replicationsnapshot.Envelope) {
			envelope.Transient.SourcePrimaryID = uuid.New()
		}},
		{name: "generation mismatch", mutate: func(envelope *replicationsnapshot.Envelope) {
			envelope.Transient.LeadershipGeneration = 8
		}},
		{name: "cluster key ref mismatch", mutate: func(envelope *replicationsnapshot.Envelope) {
			other := uuid.New()
			envelope.NodeLocal.ClusterKeyRef = &other
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			publisher, store, _, credential := newFakeSnapshotPublisher(t)
			defer credential.Destroy()
			body, declaredHash := mutatePublicationEnvelope(t, store.publication.Snapshot.Bytes, tc.mutate)
			store.publication.Snapshot.Bytes = body
			store.publication.Snapshot.ContentHash = declaredHash
			if tc.name == "incompatible version" {
				store.publication.Snapshot.SnapshotVersion = 2
			}
			result, err := publisher.Publish(context.Background(), SnapshotRequest{Credential: credential.BearerCopy()})
			if err != ErrSnapshotDenied || result.Current || len(result.Bytes) != 0 {
				t.Fatalf("integrity result = %#v, err=%v", result, err)
			}
		})
	}
}

func TestSnapshotPublisherAuthorizationAndReadinessMatrix(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*fakeSnapshotPublicationStore, *fakeSnapshotIdentityReader)
	}{
		{name: "credential revoked", mutate: func(store *fakeSnapshotPublicationStore, _ *fakeSnapshotIdentityReader) {
			at := time.Unix(1, 0)
			store.credential.CredentialRevokedAt = &at
		}},
		{name: "node revoked", mutate: func(store *fakeSnapshotPublicationStore, _ *fakeSnapshotIdentityReader) {
			at := time.Unix(2, 0)
			store.credential.NodeRevokedAt = &at
		}},
		{name: "standalone primary", mutate: func(_ *fakeSnapshotPublicationStore, reader *fakeSnapshotIdentityReader) {
			reader.current.Role = domain.TopologyRoleStandalone
		}},
		{name: "stale generation", mutate: func(_ *fakeSnapshotPublicationStore, reader *fakeSnapshotIdentityReader) {
			reader.current.LatestKnownGeneration++
		}},
		{name: "unusable cluster key", mutate: func(store *fakeSnapshotPublicationStore, _ *fakeSnapshotIdentityReader) {
			store.publication.Identity.ClusterKeyUsable = false
		}},
		{name: "database node mismatch", mutate: func(store *fakeSnapshotPublicationStore, _ *fakeSnapshotIdentityReader) {
			store.publication.Identity.NodeID = domain.NewNodeID().String()
		}},
		{name: "database cluster key mismatch", mutate: func(store *fakeSnapshotPublicationStore, _ *fakeSnapshotIdentityReader) {
			other := uuid.New()
			store.publication.Identity.ClusterKeyID = &other
		}},
		{name: "loaded cluster key id missing", mutate: func(_ *fakeSnapshotPublicationStore, reader *fakeSnapshotIdentityReader) {
			reader.current.ClusterKeyID = nil
		}},
		{name: "source primary mismatch", mutate: func(store *fakeSnapshotPublicationStore, _ *fakeSnapshotIdentityReader) {
			store.publication.Snapshot.SourcePrimaryID = domain.NewInstallationID().String()
		}},
		{name: "no complete candidate", mutate: func(store *fakeSnapshotPublicationStore, _ *fakeSnapshotIdentityReader) {
			store.publication.Snapshot = configuration.SnapshotPublicationRecord{}
		}},
		{name: "malformed metadata", mutate: func(store *fakeSnapshotPublicationStore, _ *fakeSnapshotIdentityReader) {
			store.publication.Snapshot.ContentHash = "UPPERCASE"
		}},
		{name: "hash mismatch", mutate: func(store *fakeSnapshotPublicationStore, _ *fakeSnapshotIdentityReader) {
			store.publication.Snapshot.Bytes = []byte(`{"tampered":true}`)
		}},
		{name: "malformed bytes", mutate: func(store *fakeSnapshotPublicationStore, _ *fakeSnapshotIdentityReader) {
			store.publication.Snapshot.Bytes = []byte("{")
			sum := sha256.Sum256(store.publication.Snapshot.Bytes)
			store.publication.Snapshot.ContentHash = hex.EncodeToString(sum[:])
		}},
		{name: "oversize bound", mutate: func(store *fakeSnapshotPublicationStore, _ *fakeSnapshotIdentityReader) {
			store.publication.Snapshot.Bytes = append(store.publication.Snapshot.Bytes, make([]byte, 32)...)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			publisher, store, reader, credential := newFakeSnapshotPublisher(t)
			defer credential.Destroy()
			tc.mutate(store, reader)
			if tc.name == "oversize bound" {
				publisher = NewSnapshotPublisher(store, reader, SnapshotPublisherOptions{MaxBytes: 4})
			}
			got, err := publisher.Publish(context.Background(), SnapshotRequest{Credential: credential.BearerCopy()})
			if err != ErrSnapshotDenied {
				t.Fatalf("error = %v, want %v", err, ErrSnapshotDenied)
			}
			if got.Current || len(got.Bytes) != 0 {
				t.Fatalf("denied result exposed snapshot body: %#v", got)
			}
		})
	}
	invalid, _, _, credential := newFakeSnapshotPublisher(t)
	defer credential.Destroy()
	if _, err := invalid.Publish(context.Background(), SnapshotRequest{Credential: "not-a-pcnode1"}); err != ErrSnapshotDenied {
		t.Fatalf("malformed credential error = %v, want %v", err, ErrSnapshotDenied)
	}
}

func TestSnapshotPublisherRejectsUnloadedIdentityService(t *testing.T) {
	publisher, store, _, credential := newFakeSnapshotPublisher(t)
	defer credential.Destroy()
	unloaded := identity.NewService(nil)
	publisher = NewSnapshotPublisher(store, unloaded, SnapshotPublisherOptions{})
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("unloaded identity publisher panicked: %v", recovered)
		}
	}()
	if _, err := publisher.Publish(context.Background(), SnapshotRequest{Credential: credential.BearerCopy()}); err != ErrSnapshotDenied {
		t.Fatalf("unloaded identity error = %v, want %v", err, ErrSnapshotDenied)
	}
}

func TestSnapshotPublisherAfterHashAndCopyIsolation(t *testing.T) {
	publisher, store, _, credential := newFakeSnapshotPublisher(t)
	defer credential.Destroy()
	hash := store.publication.Snapshot.ContentHash
	current, err := publisher.Publish(context.Background(), SnapshotRequest{Credential: credential.BearerCopy(), After: hash})
	if err != nil {
		t.Fatalf("current Publish: %v", err)
	}
	if !current.Current || len(current.Bytes) != 0 || current.Metadata.ContentHash != hash {
		t.Fatalf("current result = %#v", current)
	}
	behind, err := publisher.Publish(context.Background(), SnapshotRequest{
		Credential: credential.BearerCopy(),
		After:      strings.Repeat("0", SnapshotContentHashHexLength),
	})
	if err != nil {
		t.Fatalf("behind Publish: %v", err)
	}
	if behind.Current || len(behind.Bytes) == 0 {
		t.Fatal("behind request did not return the latest bytes")
	}
	behind.Bytes[0] ^= 0xff
	again, err := publisher.Publish(context.Background(), SnapshotRequest{Credential: credential.BearerCopy()})
	if err != nil {
		t.Fatalf("repeat Publish: %v", err)
	}
	if again.Bytes[0] == behind.Bytes[0] {
		t.Fatal("publisher response shares mutable body storage")
	}
}

func newFakeSnapshotPublisher(t *testing.T) (*SnapshotPublisher, *fakeSnapshotPublicationStore, *fakeSnapshotIdentityReader, *NodeCredential) {
	t.Helper()
	primaryID := domain.NewInstallationID()
	primaryNodeID := domain.NewNodeID()
	clusterKeyID := uuid.New()
	body, hash := canonicalPublicationEnvelope(t, primaryID, primaryNodeID, domain.TopologyRolePrimary, 7, 0, &clusterKeyID)
	credential, err := NewNodeCredential("00112233-4455-4677-8899-aabbccddeeff", make([]byte, NodeCredentialSecretBytes))
	if err != nil {
		t.Fatalf("NewNodeCredential: %v", err)
	}
	store := &fakeSnapshotPublicationStore{
		credential: configuration.SnapshotPublicationCredentialRecord{
			ID: credential.ID(), NodeID: "ffeeddcc-bbaa-4999-8877-665544332211",
			CredentialHash: credential.Hash(), HashVersion: NodeCredentialHashVersion,
			PrimaryID: string(primaryID),
		},
		publication: configuration.SnapshotPublicationResult{
			Identity: configuration.SnapshotPublicationIdentityRecord{
				InstallationID: string(primaryID), NodeID: string(primaryNodeID), Role: domain.TopologyRolePrimary,
				LeadershipGeneration: 7, LatestKnownGeneration: 7, ClusterKeyID: &clusterKeyID, ClusterKeyUsable: true,
			},
			Snapshot: configuration.SnapshotPublicationRecord{
				SnapshotID: "11112222-3333-4444-8999-aabbccddeeff", Bytes: body,
				ContentHash: hash, SnapshotVersion: 1, ReplicationVersion: 1, ProofComplete: true,
				RevisionID: "22223333-4444-4555-8999-aabbccddeeff", RevisionNumber: 47,
				SourcePrimaryID: string(primaryID), LeadershipGeneration: 7,
				ApplyJobID: "33334444-5555-4666-8999-aabbccddeeff",
				AppliedAt:  time.Date(2026, 10, 2, 3, 4, 5, 0, time.UTC),
			},
		},
	}
	reader := &fakeSnapshotIdentityReader{current: identity.Identity{
		InstallationID: primaryID, NodeID: primaryNodeID, Role: domain.TopologyRolePrimary,
		LeadershipGeneration: 7, LatestKnownGeneration: 7, ClusterKeyID: &clusterKeyID,
	}}
	return NewSnapshotPublisher(store, reader, SnapshotPublisherOptions{}), store, reader, &credential
}
