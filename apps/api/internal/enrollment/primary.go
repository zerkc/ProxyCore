package enrollment

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

const DefaultGrantTTL = 15 * time.Minute

// NodeCredentialMaterial is supplied by the sync credential authority. The
// primary service only uses Secret transiently to seal the bootstrap grant;
// only Hash and HashVersion cross the persistence boundary.
type NodeCredentialMaterial struct {
	ID          string
	Secret      []byte
	Hash        string
	HashVersion string
}

func (m NodeCredentialMaterial) String() string { return "[node credential material redacted]" }

func (m NodeCredentialMaterial) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, m.String())
}

func (m NodeCredentialMaterial) MarshalJSON() ([]byte, error) {
	return json.Marshal(m.String())
}

type NodeCredentialIssuer interface {
	IssueNodeCredential(context.Context) (NodeCredentialMaterial, error)
	ValidateNodeCredential(NodeCredentialMaterial) error
}

type NodeCredentialIssuerFunc struct {
	Issue    func(context.Context) (NodeCredentialMaterial, error)
	Validate func(NodeCredentialMaterial) error
}

func (f NodeCredentialIssuerFunc) IssueNodeCredential(ctx context.Context) (NodeCredentialMaterial, error) {
	if f.Issue == nil {
		return NodeCredentialMaterial{}, ErrEnrollmentDenied
	}
	return f.Issue(ctx)
}

func (f NodeCredentialIssuerFunc) ValidateNodeCredential(material NodeCredentialMaterial) error {
	if f.Validate == nil {
		return ErrEnrollmentDenied
	}
	return f.Validate(material)
}

// PrimaryGrantRequest is the complete binding accepted by the primary
// exchange. EnrollmentToken is held by the caller and is never passed to the
// configuration store.
type PrimaryGrantRequest struct {
	EnrollmentToken               string
	AttemptID                     string
	TargetInstallationID          string
	TargetNodeID                  string
	RecipientPublicKey            BootstrapRecipientPublicKey
	VerifiedPreviewDigest         []byte
	ExpectedPrimaryInstallationID string
	ExpectedLeadershipGeneration  uint64
	GrantTTL                      time.Duration
}

func (r PrimaryGrantRequest) String() string { return "[primary grant request redacted]" }

func (r PrimaryGrantRequest) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, r.String())
}

func (r PrimaryGrantRequest) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.String())
}

type PrimaryGrantResponse struct {
	SealedBootstrapPayload []byte
	PayloadHash            string
	ExpiresAt              time.Time
}

type SealBootstrapFunc func(BootstrapRecipientPublicKey, BootstrapBinding, BootstrapGrant) ([]byte, error)

type PrimaryServiceOptions struct {
	Now              func() time.Time
	GrantTTL         time.Duration
	CredentialIssuer NodeCredentialIssuer
	Identity         IdentityActivation
	SealBootstrap    SealBootstrapFunc
}

type PrimaryService struct {
	store            configuration.PrimaryGrantStore
	now              func() time.Time
	grantTTL         time.Duration
	credentialIssuer NodeCredentialIssuer
	identity         IdentityActivation
	sealBootstrap    SealBootstrapFunc
}

func NewPrimaryService(store configuration.PrimaryGrantStore, opts PrimaryServiceOptions) *PrimaryService {
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	if opts.GrantTTL == 0 {
		opts.GrantTTL = DefaultGrantTTL
	}
	if opts.SealBootstrap == nil {
		opts.SealBootstrap = SealBootstrap
	}
	return &PrimaryService{
		store:            store,
		now:              opts.Now,
		grantTTL:         opts.GrantTTL,
		credentialIssuer: opts.CredentialIssuer,
		identity:         opts.Identity,
		sealBootstrap:    opts.SealBootstrap,
	}
}

func (s *PrimaryService) IssueGrant(ctx context.Context, request PrimaryGrantRequest) (PrimaryGrantResponse, error) {
	if s == nil || s.store == nil || ctx == nil {
		return PrimaryGrantResponse{}, ErrEnrollmentDenied
	}
	if err := ctx.Err(); err != nil {
		return PrimaryGrantResponse{}, ErrEnrollmentDenied
	}
	selector, secret, ok := parseToken(request.EnrollmentToken)
	if !ok {
		return PrimaryGrantResponse{}, ErrEnrollmentDenied
	}
	defer zeroBytes(secret)
	if !validPrimaryGrantRequest(request) {
		return PrimaryGrantResponse{}, ErrEnrollmentDenied
	}
	grantTTL := request.GrantTTL
	if grantTTL == 0 {
		grantTTL = s.grantTTL
	}
	if grantTTL <= 0 {
		return PrimaryGrantResponse{}, ErrEnrollmentDenied
	}
	tokenHash := hashSecret(secret)
	previewDigest := append([]byte(nil), request.VerifiedPreviewDigest...)
	defer zeroBytes(previewDigest)
	recipientWire, err := request.RecipientPublicKey.Wire()
	if err != nil {
		return PrimaryGrantResponse{}, ErrEnrollmentDenied
	}
	now := s.now().UTC()
	sealBootstrap := s.sealBootstrap
	if sealBootstrap == nil {
		sealBootstrap = SealBootstrap
	}
	var response PrimaryGrantResponse
	activate := func(runCtx context.Context) (domain.TopologyRole, error) {
		ctx := runCtx
		var committedRole domain.TopologyRole
		err := s.store.WithPrimaryGrant(ctx, func(tx configuration.PrimaryGrantTransaction) error {
			token, err := tx.LockEnrollmentToken(ctx, selector)
			if err != nil || !validTokenSecret(token, tokenHash) {
				return ErrEnrollmentDenied
			}
			identity, err := tx.LockPrimaryIdentity(ctx)
			if err != nil || !validExpectedPrimary(identity, request) {
				return ErrEnrollmentDenied
			}
			if token.ConsumedAt != nil {
				if token.ConsumedByAttemptID != request.AttemptID {
					return ErrEnrollmentDenied
				}
				grant, err := tx.GetEnrollmentGrant(ctx, request.AttemptID)
				if err != nil || !matchesRetry(grant, token.ID, request, identity, recipientWire, previewDigest, now) {
					return ErrEnrollmentDenied
				}
				response = responseFromGrant(grant)
				committedRole = identity.Role
				return nil
			}
			if token.ConsumedAt != nil || token.ConsumedByAttemptID != "" ||
				!token.ExpiresAt.After(now) || token.RevokedAt != nil || s.credentialIssuer == nil {
				return ErrEnrollmentDenied
			}
			keyMaterial, err := tx.LoadOrCreateClusterKEK(ctx, clusterKeyID(identity))
			if err != nil {
				return ErrEnrollmentDenied
			}
			defer keyMaterial.Destroy()
			clusterKey := keyMaterial.CopyBytes()
			defer zeroBytes(clusterKey)
			if len(clusterKey) != bootstrapKEKBytes {
				return ErrEnrollmentDenied
			}
			credential, err := s.credentialIssuer.IssueNodeCredential(ctx)
			if err != nil || !validNodeCredentialMaterial(credential) || s.credentialIssuer.ValidateNodeCredential(credential) != nil {
				zeroBytes(credential.Secret)
				return ErrEnrollmentDenied
			}
			defer zeroBytes(credential.Secret)
			grantExpiresAt := now.Add(grantTTL)
			if !grantExpiresAt.After(now) {
				return ErrEnrollmentDenied
			}
			binding := BootstrapBinding{
				ProtocolVersion:             BootstrapProtocolVersion,
				AttemptID:                   request.AttemptID,
				SourcePrimaryInstallationID: request.ExpectedPrimaryInstallationID,
				TargetInstallationID:        request.TargetInstallationID,
				TargetNodeID:                request.TargetNodeID,
				LeadershipGeneration:        request.ExpectedLeadershipGeneration,
				VerifiedPreviewDigest:       previewDigest,
			}
			grant, err := NewBootstrapGrant(BootstrapGrantMetadata{
				ProtocolVersion:             binding.ProtocolVersion,
				CredentialID:                credential.ID,
				ClusterKeyID:                keyMaterial.ID(),
				SourcePrimaryInstallationID: binding.SourcePrimaryInstallationID,
				TargetInstallationID:        binding.TargetInstallationID,
				TargetNodeID:                binding.TargetNodeID,
				AttemptID:                   binding.AttemptID,
				LeadershipGeneration:        binding.LeadershipGeneration,
				ExpiresAtUnix:               grantExpiresAt.Unix(),
			}, credential.Secret, clusterKey)
			if err != nil {
				return ErrEnrollmentDenied
			}
			defer grant.Destroy()
			sealed, err := sealBootstrap(request.RecipientPublicKey, binding, grant)
			if err != nil {
				return ErrEnrollmentDenied
			}
			payloadSum := sha256.Sum256(sealed)
			payloadHash := hex.EncodeToString(payloadSum[:])
			if err := tx.CreateNodeCredential(ctx, configuration.NodeCredentialRecord{
				ID:             credential.ID,
				NodeID:         request.TargetNodeID,
				CredentialHash: credential.Hash,
				HashVersion:    credential.HashVersion,
				CreatedAt:      now,
			}); err != nil {
				return ErrEnrollmentDenied
			}
			if err := tx.CreateEnrolledNode(ctx, configuration.EnrolledNodeRecord{
				NodeID:             request.TargetNodeID,
				InstallationID:     request.TargetInstallationID,
				PrimaryID:          request.ExpectedPrimaryInstallationID,
				CredentialID:       credential.ID,
				CreatedByAttemptID: request.AttemptID,
				EnrolledAt:         now,
			}); err != nil {
				return ErrEnrollmentDenied
			}
			if err := tx.CreateEnrollmentGrant(ctx, configuration.EnrollmentGrantRecord{
				AttemptID:              request.AttemptID,
				TokenID:                token.ID,
				InstallationID:         request.TargetInstallationID,
				NodeID:                 request.TargetNodeID,
				PrimaryID:              request.ExpectedPrimaryInstallationID,
				PrimaryGeneration:      request.ExpectedLeadershipGeneration,
				VerifiedPreviewDigest:  hex.EncodeToString(previewDigest),
				NodeEphemeralPublicKey: string(recipientWire),
				SealedBootstrapPayload: append([]byte(nil), sealed...),
				PayloadHash:            payloadHash,
				CreatedAt:              now,
				ExpiresAt:              grantExpiresAt,
			}); err != nil {
				return ErrEnrollmentDenied
			}
			if err := tx.ConsumeEnrollmentToken(ctx, token.ID, request.AttemptID, now); err != nil {
				return ErrEnrollmentDenied
			}
			if identity.Role == domain.TopologyRolePrimary {
				if err := tx.TransitionPrimaryToWithNodes(ctx, identity.LeadershipGeneration); err != nil {
					return ErrEnrollmentDenied
				}
				committedRole = domain.TopologyRolePrimaryWithNodes
			} else {
				committedRole = identity.Role
			}
			response = PrimaryGrantResponse{
				SealedBootstrapPayload: append([]byte(nil), sealed...),
				PayloadHash:            payloadHash,
				ExpiresAt:              grantExpiresAt,
			}
			return nil
		})
		return committedRole, err
	}
	if s.identity != nil {
		_, err = s.identity.ActivateEnrollment(ctx, activate)
	} else {
		_, err = activate(ctx)
	}
	if err != nil {
		return PrimaryGrantResponse{}, ErrEnrollmentDenied
	}
	return response, nil
}

func validPrimaryGrantRequest(request PrimaryGrantRequest) bool {
	if request.ExpectedLeadershipGeneration == 0 || len(request.VerifiedPreviewDigest) != sha256.Size {
		return false
	}
	for _, value := range []string{
		request.AttemptID,
		request.TargetInstallationID,
		request.TargetNodeID,
		request.ExpectedPrimaryInstallationID,
	} {
		if !canonicalUUID(value) {
			return false
		}
	}
	return true
}

func canonicalUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed != uuid.Nil && parsed.String() == value
}

func validTokenSecret(token configuration.EnrollmentTokenRecord, hash string) bool {
	return token.ID != "" && token.HashVersion == TokenHashVersion &&
		subtle.ConstantTimeCompare([]byte(token.Hash), []byte(hash)) == 1
}

func validExpectedPrimary(identity configuration.PrimaryIdentityRecord, request PrimaryGrantRequest) bool {
	if identity.InstallationID != request.ExpectedPrimaryInstallationID ||
		identity.LeadershipGeneration != request.ExpectedLeadershipGeneration ||
		identity.LeadershipGeneration != identity.LatestKnownGeneration {
		return false
	}
	return identity.Role == domain.TopologyRolePrimary || identity.Role == domain.TopologyRolePrimaryWithNodes
}

func clusterKeyID(identity configuration.PrimaryIdentityRecord) string {
	if identity.ClusterKeyID == nil {
		return ""
	}
	return identity.ClusterKeyID.String()
}

func validNodeCredentialMaterial(credential NodeCredentialMaterial) bool {
	if !canonicalUUID(credential.ID) || len(credential.Secret) != bootstrapCredentialBytes {
		return false
	}
	if credential.HashVersion == "" || len(credential.Hash) != sha256.Size*2 {
		return false
	}
	for _, value := range []byte(credential.Hash) {
		if !((value >= '0' && value <= '9') || (value >= 'a' && value <= 'f')) {
			return false
		}
	}
	return true
}

func matchesRetry(
	grant configuration.EnrollmentGrantRecord,
	tokenID string,
	request PrimaryGrantRequest,
	identity configuration.PrimaryIdentityRecord,
	recipientWire, previewDigest []byte,
	now time.Time,
) bool {
	if grant.AttemptID != request.AttemptID || grant.TokenID != tokenID ||
		grant.InstallationID != request.TargetInstallationID || grant.NodeID != request.TargetNodeID ||
		grant.PrimaryID != request.ExpectedPrimaryInstallationID ||
		grant.PrimaryGeneration != request.ExpectedLeadershipGeneration ||
		grant.PrimaryID != identity.InstallationID || grant.PrimaryGeneration != identity.LeadershipGeneration ||
		grant.NodeEphemeralPublicKey != string(recipientWire) ||
		grant.VerifiedPreviewDigest != hex.EncodeToString(previewDigest) ||
		!grant.ExpiresAt.After(now) || len(grant.SealedBootstrapPayload) == 0 || grant.PayloadHash == "" {
		return false
	}
	sum := sha256.Sum256(grant.SealedBootstrapPayload)
	return subtle.ConstantTimeCompare([]byte(grant.PayloadHash), []byte(hex.EncodeToString(sum[:]))) == 1
}

func responseFromGrant(grant configuration.EnrollmentGrantRecord) PrimaryGrantResponse {
	return PrimaryGrantResponse{
		SealedBootstrapPayload: append([]byte(nil), grant.SealedBootstrapPayload...),
		PayloadHash:            grant.PayloadHash,
		ExpiresAt:              grant.ExpiresAt,
	}
}
