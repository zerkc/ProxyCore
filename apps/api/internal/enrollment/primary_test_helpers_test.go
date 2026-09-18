package enrollment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
	nodesync "github.com/zerkc/ProxyCore/apps/api/internal/sync"
)

type primaryFixture struct {
	service          *PrimaryService
	request          PrimaryGrantRequest
	recipient        BootstrapRecipient
	tx               *memoryPrimaryGrantTransaction
	issuerCalls      int
	token            string
	credentialBearer string
	credentialSecret []byte
	clusterKey       []byte
	kekText          string
}

func newPrimaryFixture(t *testing.T) *primaryFixture {
	t.Helper()
	recipient, err := NewBootstrapRecipient()
	if err != nil {
		t.Fatalf("NewBootstrapRecipient: %v", err)
	}
	t.Cleanup(recipient.Destroy)
	publicKey, err := recipient.PublicKey()
	if err != nil {
		t.Fatalf("PublicKey: %v", err)
	}
	attemptID := "11112222-3333-4444-8999-aabbccddeeff"
	primaryID := "22223333-4444-4555-8999-aabbccddeeff"
	installationID := "33334444-5555-4666-8999-aabbccddeeff"
	nodeID := "44445555-6666-4777-8999-aabbccddeeff"
	token := testPlaintextToken(t, bytes.Repeat([]byte{0x41}, tokenSecretLen))
	clusterKey := bytes.Repeat([]byte{0x5c}, bootstrapKEKBytes)
	credentialSecret := bytes.Repeat([]byte{0x6d}, nodesync.NodeCredentialSecretBytes)
	credential, err := nodesync.NewNodeCredential("55556666-7777-4888-8999-aabbccddeeff", credentialSecret)
	if err != nil {
		t.Fatalf("NewNodeCredential: %v", err)
	}
	credentialBearer := credential.BearerCopy()
	credentialID := credential.ID()
	credentialHash := credential.Hash()
	credential.Destroy()
	now := time.Date(2026, 10, 2, 3, 4, 5, 0, time.UTC)
	tx := &memoryPrimaryGrantTransaction{
		token: configuration.EnrollmentTokenRecord{
			ID:          "66667777-8888-4999-8aaa-bbbbccccdddd",
			Hash:        hashSecret(bytes.Repeat([]byte{0x41}, tokenSecretLen)),
			HashVersion: TokenHashVersion,
			ExpiresAt:   now.Add(time.Hour),
		},
		identity: configuration.PrimaryIdentityRecord{
			InstallationID:        primaryID,
			Role:                  domain.TopologyRolePrimary,
			LeadershipGeneration:  7,
			LatestKnownGeneration: 7,
		},
		clusterKeyID: "77778888-9999-4aaa-8bbb-ccccddddeeee",
		clusterKey:   clusterKey,
		credential: configuration.NodeCredentialRecord{
			ID:             credentialID,
			NodeID:         nodeID,
			CredentialHash: credentialHash,
			HashVersion:    nodesync.NodeCredentialHashVersion,
			CreatedAt:      now,
		},
	}
	store := &memoryPrimaryGrantStore{tx: tx}
	fixture := &primaryFixture{
		recipient:        recipient,
		tx:               tx,
		token:            token,
		credentialBearer: credentialBearer,
		credentialSecret: credentialSecret,
		clusterKey:       clusterKey,
		kekText:          string(clusterKey),
		request: PrimaryGrantRequest{
			EnrollmentToken:               token,
			AttemptID:                     attemptID,
			TargetInstallationID:          installationID,
			TargetNodeID:                  nodeID,
			RecipientPublicKey:            publicKey,
			VerifiedPreviewDigest:         bytes.Repeat([]byte{0x29}, sha256.Size),
			ExpectedPrimaryInstallationID: primaryID,
			ExpectedLeadershipGeneration:  7,
			GrantTTL:                      20 * time.Minute,
		},
	}
	fixture.service = NewPrimaryService(store, PrimaryServiceOptions{
		Now: func() time.Time { return now },
		CredentialIssuer: testNodeCredentialIssuer(func(context.Context) (NodeCredentialMaterial, error) {
			fixture.issuerCalls++
			material, err := nodesync.NewNodeCredential(tx.credential.ID, credentialSecret)
			if err != nil {
				return NodeCredentialMaterial{}, err
			}
			secret := material.CopyBytes()
			bearer := material.BearerCopy()
			material.Destroy()
			if fixture.credentialBearer == "" {
				fixture.credentialBearer = bearer
			}
			return NodeCredentialMaterial{
				ID:          tx.credential.ID,
				Secret:      secret,
				Hash:        tx.credential.CredentialHash,
				HashVersion: tx.credential.HashVersion,
			}, nil
		}),
	})
	return fixture
}

func testPlaintextToken(t *testing.T, secret []byte) string {
	t.Helper()
	return tokenPrefix + "_" + strings.Repeat("a1", tokenSelectorLen) + "_" + base64.RawURLEncoding.EncodeToString(secret)
}

func testBindingFromRequest(request PrimaryGrantRequest) BootstrapBinding {
	return BootstrapBinding{
		ProtocolVersion:             BootstrapProtocolVersion,
		AttemptID:                   request.AttemptID,
		SourcePrimaryInstallationID: request.ExpectedPrimaryInstallationID,
		TargetInstallationID:        request.TargetInstallationID,
		TargetNodeID:                request.TargetNodeID,
		LeadershipGeneration:        request.ExpectedLeadershipGeneration,
		VerifiedPreviewDigest:       request.VerifiedPreviewDigest,
	}
}

func stringsContainAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if candidate != "" && bytes.Contains([]byte(value), []byte(candidate)) {
			return true
		}
	}
	return false
}

func testNodeCredentialIssuer(issue func(context.Context) (NodeCredentialMaterial, error)) NodeCredentialIssuerFunc {
	return NodeCredentialIssuerFunc{Issue: issue, Validate: validateTestNodeCredentialMaterial}
}

func validateTestNodeCredentialMaterial(material NodeCredentialMaterial) error {
	if material.HashVersion != nodesync.NodeCredentialHashVersion {
		return ErrEnrollmentDenied
	}
	credential, err := nodesync.NewNodeCredential(material.ID, material.Secret)
	if err != nil {
		return ErrEnrollmentDenied
	}
	defer credential.Destroy()
	if credential.Hash() != material.Hash {
		return ErrEnrollmentDenied
	}
	return nil
}

type memoryPrimaryGrantStore struct {
	mu *sync.Mutex
	tx *memoryPrimaryGrantTransaction
}

func (s *memoryPrimaryGrantStore) WithPrimaryGrant(_ context.Context, fn func(configuration.PrimaryGrantTransaction) error) error {
	if s.mu != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
	}
	return fn(s.tx)
}

type memoryPrimaryGrantTransaction struct {
	token        configuration.EnrollmentTokenRecord
	identity     configuration.PrimaryIdentityRecord
	clusterKeyID string
	clusterKey   []byte
	grant        configuration.EnrollmentGrantRecord
	credential   configuration.NodeCredentialRecord
	node         configuration.EnrolledNodeRecord
	clusterLoads int
	clusterErr   error
	transition   func()
}

func (t *memoryPrimaryGrantTransaction) LockEnrollmentToken(context.Context, string) (configuration.EnrollmentTokenRecord, error) {
	return t.token, nil
}
func (t *memoryPrimaryGrantTransaction) LockPrimaryIdentity(context.Context) (configuration.PrimaryIdentityRecord, error) {
	return t.identity, nil
}
func (t *memoryPrimaryGrantTransaction) LoadOrCreateClusterKEK(context.Context, string) (configuration.ClusterKeyMaterial, error) {
	t.clusterLoads++
	if t.clusterErr != nil {
		return configuration.ClusterKeyMaterial{}, t.clusterErr
	}
	return configuration.NewClusterKeyMaterial(t.clusterKeyID, t.clusterKey)
}
func (t *memoryPrimaryGrantTransaction) GetEnrollmentGrant(context.Context, string) (configuration.EnrollmentGrantRecord, error) {
	if t.grant.AttemptID == "" {
		return configuration.EnrollmentGrantRecord{}, configuration.ErrEnrollmentGrantStore
	}
	return t.grant, nil
}
func (t *memoryPrimaryGrantTransaction) CreateNodeCredential(_ context.Context, credential configuration.NodeCredentialRecord) error {
	t.credential = credential
	return nil
}
func (t *memoryPrimaryGrantTransaction) CreateEnrolledNode(_ context.Context, node configuration.EnrolledNodeRecord) error {
	t.node = node
	return nil
}
func (t *memoryPrimaryGrantTransaction) CreateEnrollmentGrant(_ context.Context, grant configuration.EnrollmentGrantRecord) error {
	t.grant = grant
	return nil
}
func (t *memoryPrimaryGrantTransaction) ConsumeEnrollmentToken(_ context.Context, _, attemptID string, at time.Time) error {
	t.token.ConsumedAt = &at
	t.token.ConsumedByAttemptID = attemptID
	return nil
}
func (t *memoryPrimaryGrantTransaction) TransitionPrimaryToWithNodes(context.Context, uint64) error {
	t.identity.Role = domain.TopologyRolePrimaryWithNodes
	if t.transition != nil {
		t.transition()
	}
	return nil
}

type liveIdentityStore struct {
	current identity.Identity
}

func (s *liveIdentityStore) Get(context.Context) (identity.Identity, error) { return s.current, nil }
func (s *liveIdentityStore) Ensure(context.Context, domain.InstallationID, domain.NodeID) (identity.Identity, bool, error) {
	return s.current, false, nil
}
func (s *liveIdentityStore) UpdateRole(_ context.Context, role domain.TopologyRole) error {
	s.current.Role = role
	return nil
}
func (s *liveIdentityStore) UpdateLeadershipGeneration(_ context.Context, generation domain.LeadershipGeneration) error {
	s.current.LeadershipGeneration = generation
	if generation > s.current.LatestKnownGeneration {
		s.current.LatestKnownGeneration = generation
	}
	return nil
}
func (s *liveIdentityStore) UpdateLatestKnownGeneration(_ context.Context, generation domain.LeadershipGeneration) error {
	if generation > s.current.LatestKnownGeneration {
		s.current.LatestKnownGeneration = generation
	}
	return nil
}
func (s *liveIdentityStore) UpdateClusterKeyID(_ context.Context, keyID *uuid.UUID) error {
	s.current.ClusterKeyID = keyID
	return nil
}

var _ configuration.PrimaryGrantStore = (*memoryPrimaryGrantStore)(nil)
var _ configuration.PrimaryGrantTransaction = (*memoryPrimaryGrantTransaction)(nil)
