package httpserver_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/cluster"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
	syncpublication "github.com/zerkc/ProxyCore/apps/api/internal/sync"
)

// confirmFixture is a fake primary + local acceptor around the real bootstrap
// sealing/opening so the handler is exercised with real recipient/binding code.
type confirmFixture struct {
	t        *testing.T
	envelope *replicationsnapshot.Envelope
	cache    *enrollment.DraftCache
	handler  http.Handler
	draftID  string
	token    string

	mu              sync.Mutex
	exchangeErr     error
	exchangeTokens  []string
	exchangeReqs    []syncpublication.ExchangeRequest
	sealedByAttempt map[string]syncpublication.ExchangeResult
	acceptErrs      []error
	accepts         int
	keys            map[string][]byte
	validateKEKs    int
	convertCalls    int
	// reEnrollEligible is what the converter reports for the live identity
	// (the "explicitly unlinked" exception); false by default.
	reEnrollEligible bool
	convertErr       error
	localInstall     domain.InstallationID
	localNode        domain.NodeID
	loadErr          error
	localRole        domain.TopologyRole
	localKeyID       string
	convertKeyIDs    []*uuid.UUID
	bindings         []configuration.NodeSyncBinding
	boundHashes      []string
	bindErr          error
	exchangeCAs      []string
	trusts           []trustRecord
	trustErr         error
	order            []string
}

type trustRecord struct{ origin, caPEM string }

type kekValidatorFunc func(context.Context, replicationsnapshot.Envelope, *cluster.KEK) replicationsnapshot.ValidationResult

func (f kekValidatorFunc) Validate(context.Context, replicationsnapshot.Envelope) replicationsnapshot.ValidationResult {
	// Strict validation without a KEK must fail closed, as the real validator does.
	return replicationsnapshot.ValidationResult{OK: false, Issues: []replicationsnapshot.ValidationIssue{{Code: "NO_KEY"}}}
}

func (f kekValidatorFunc) ValidateWithKEK(ctx context.Context, e replicationsnapshot.Envelope, k *cluster.KEK) replicationsnapshot.ValidationResult {
	return f(ctx, e, k)
}

func (f kekValidatorFunc) ValidatePreview(context.Context, replicationsnapshot.Envelope) replicationsnapshot.ValidationResult {
	return replicationsnapshot.ValidationResult{OK: true}
}

type fakeConverter struct{ f *confirmFixture }

func (c fakeConverter) CanEnrollAsNode() bool { return c.f.reEnrollEligible }

func (c fakeConverter) Convert(_ context.Context, in enrollment.NodeConversionInput) (enrollment.NodeConversionResult, error) {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.convertCalls++
	c.f.convertKeyIDs = append(c.f.convertKeyIDs, in.ClusterKeyID)
	if c.f.convertErr != nil {
		return enrollment.NodeConversionResult{}, c.f.convertErr
	}
	return enrollment.NodeConversionResult{
		ArchiveID: uuid.New(), ApplyJobID: uuid.New(),
		Identity: identity.Identity{NodeID: in.LocalNodeID, Role: domain.TopologyRoleNode, LeadershipGeneration: 1},
	}, nil
}

func newConfirmFixture(t *testing.T, withToken bool) *confirmFixture {
	t.Helper()
	f := &confirmFixture{
		t: t, envelope: workflowTestEnvelope(t), token: workflowTestToken(),
		sealedByAttempt: map[string]syncpublication.ExchangeResult{}, keys: map[string][]byte{},
		localInstall: domain.NewInstallationID(), localNode: domain.NewNodeID(),
		localRole: domain.TopologyRoleStandalone,
	}
	f.envelope.Transient.LeadershipGeneration = 3
	hash, err := f.envelope.ExpectedHash()
	if err != nil {
		t.Fatal(err)
	}
	f.envelope.ContentHash = hash
	f.cache = enrollment.NewDraftCache(enrollment.DraftCacheOptions{})
	overlay := enrollment.NodeLocalOverlay{NodeID: f.localNode, Ingress: domain.Ingress{IPv4: "192.0.2.7"}, Role: domain.TopologyRoleNode}
	const selector = "0123456789abcdef0123456789abcdef"
	var draft enrollment.Draft
	if withToken {
		draft, err = f.cache.PutWithPrimaryURLAndToken(selector, "https://primary.example:3443/", f.envelope, overlay, f.token)
	} else {
		draft, err = f.cache.PutWithPrimaryURL(selector, "https://primary.example:3443/", f.envelope, overlay)
	}
	if err != nil {
		t.Fatal(err)
	}
	f.draftID = draft.DraftID
	f.handler = httpserver.NewEnrollmentIdentityMux(httpserver.EnrollmentIdentityHandlerOptions{
		Certificate: func(context.Context) (*x509.Certificate, error) {
			return &x509.Certificate{DNSNames: []string{"primary.example"}}, nil
		},
		Workflow: &httpserver.EnrollmentWorkflowHandlerOptions{
			Cache:       f.cache,
			VerifyToken: func(context.Context, string) error { return nil },
			Validator: kekValidatorFunc(func(_ context.Context, _ replicationsnapshot.Envelope, kek *cluster.KEK) replicationsnapshot.ValidationResult {
				f.mu.Lock()
				defer f.mu.Unlock()
				if kek == nil {
					return replicationsnapshot.ValidationResult{OK: false}
				}
				f.validateKEKs++
				return replicationsnapshot.ValidationResult{OK: true}
			}),
			Identity: func(context.Context) (httpserver.EnrollmentLocalIdentity, error) {
				f.mu.Lock()
				defer f.mu.Unlock()
				return httpserver.EnrollmentLocalIdentity{InstallationID: f.localInstall, NodeID: f.localNode, Role: f.localRole, ClusterKeyID: f.localKeyID}, nil
			},
			LocalIngress:   func(context.Context) (domain.Ingress, error) { return domain.Ingress{IPv4: "192.0.2.7"}, nil },
			Converter:      fakeConverter{f},
			ExchangeGrant:  f.exchange,
			AcceptGrant:    f.accept,
			LoadClusterKEK: f.loadKEK,
			RecordPrimaryTrust: func(_ context.Context, origin, caPEM string) error {
				f.mu.Lock()
				defer f.mu.Unlock()
				if f.trustErr != nil {
					return f.trustErr
				}
				f.trusts = append(f.trusts, trustRecord{origin, caPEM})
				f.order = append(f.order, "trust")
				return nil
			},
			RecordSyncBinding: func(_ context.Context, binding configuration.NodeSyncBinding, appliedHash string) error {
				f.mu.Lock()
				defer f.mu.Unlock()
				f.order = append(f.order, "bind")
				if f.bindErr != nil {
					return f.bindErr
				}
				f.bindings = append(f.bindings, binding)
				f.boundHashes = append(f.boundHashes, appliedHash)
				return nil
			},
		},
	})
	return f
}

// exchange plays the primary: same attempt -> same sealed payload (exact retry).
func (f *confirmFixture) exchange(ctx context.Context, _ string, token string, req syncpublication.ExchangeRequest) (syncpublication.ExchangeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ca, _ := syncpublication.TrustOverrideFor(ctx, "https://primary.example:3443")
	f.exchangeCAs = append(f.exchangeCAs, ca)
	f.exchangeTokens = append(f.exchangeTokens, token)
	f.exchangeReqs = append(f.exchangeReqs, req)
	if f.exchangeErr != nil {
		return syncpublication.ExchangeResult{}, f.exchangeErr
	}
	if previous, ok := f.sealedByAttempt[req.AttemptID]; ok {
		return previous, nil
	}
	pub, err := enrollment.ParseBootstrapRecipientPublicKey(req.RecipientPublicKey)
	if err != nil {
		return syncpublication.ExchangeResult{}, syncpublication.ErrExchangeInvalid
	}
	digest, err := hex.DecodeString(req.PreviewDigest)
	if err != nil {
		return syncpublication.ExchangeResult{}, syncpublication.ErrExchangeInvalid
	}
	binding := enrollment.BootstrapBinding{
		ProtocolVersion: enrollment.BootstrapProtocolVersion, AttemptID: req.AttemptID,
		SourcePrimaryInstallationID: req.PrimaryInstallationID, TargetInstallationID: req.InstallationID,
		TargetNodeID: req.NodeID, LeadershipGeneration: req.LeadershipGeneration, VerifiedPreviewDigest: digest,
	}
	credential, kek := make([]byte, 32), make([]byte, 32)
	_, _ = rand.Read(credential)
	_, _ = rand.Read(kek)
	grant, err := enrollment.NewBootstrapGrant(enrollment.BootstrapGrantMetadata{
		ProtocolVersion: binding.ProtocolVersion, CredentialID: uuid.NewString(), ClusterKeyID: uuid.NewString(),
		SourcePrimaryInstallationID: binding.SourcePrimaryInstallationID, TargetInstallationID: binding.TargetInstallationID,
		TargetNodeID: binding.TargetNodeID, AttemptID: binding.AttemptID, LeadershipGeneration: binding.LeadershipGeneration,
		ExpiresAtUnix: time.Now().Add(time.Hour).Unix(),
	}, credential, kek)
	if err != nil {
		f.t.Fatal(err)
	}
	defer grant.Destroy()
	sealed, err := enrollment.SealBootstrap(pub, binding, grant)
	if err != nil {
		f.t.Fatal(err)
	}
	result := syncpublication.ExchangeResult{SealedPayload: sealed, PayloadHash: sha256Hex(sealed), ExpiresAt: time.Now().Add(time.Hour)}
	f.sealedByAttempt[req.AttemptID] = result
	return result, nil
}

// accept plays BootstrapAcceptor: really opens the sealed payload with the
// recipient and binding, then "persists" the KEK in memory.
func (f *confirmFixture) accept(_ context.Context, recipient *enrollment.BootstrapRecipient, binding enrollment.BootstrapBinding, sealed []byte, hash string) (enrollment.AcceptedBootstrap, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accepts++
	if len(f.acceptErrs) > 0 {
		err := f.acceptErrs[0]
		f.acceptErrs = f.acceptErrs[1:]
		if err != nil {
			return enrollment.AcceptedBootstrap{}, err
		}
	}
	if sha256Hex(sealed) != hash {
		return enrollment.AcceptedBootstrap{}, enrollment.ErrBootstrapDenied
	}
	grant, err := recipient.OpenBootstrap(binding, sealed)
	if err != nil {
		return enrollment.AcceptedBootstrap{}, enrollment.ErrBootstrapDenied
	}
	defer grant.Destroy()
	f.keys[grant.Metadata.ClusterKeyID] = grant.Secrets.ClusterKEK.CopyBytes()
	return enrollment.AcceptedBootstrap{
		CredentialID: grant.Metadata.CredentialID, ClusterKeyID: grant.Metadata.ClusterKeyID,
		SourcePrimaryInstallationID: grant.Metadata.SourcePrimaryInstallationID,
		LeadershipGeneration:        grant.Metadata.LeadershipGeneration,
	}, nil
}

func (f *confirmFixture) loadKEK(_ context.Context, id uuid.UUID) (*cluster.KEK, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	key, ok := f.keys[id.String()]
	if !ok {
		return nil, errors.New("no key")
	}
	return cluster.NewKEK(key)
}

func (f *confirmFixture) confirm() *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"draftId": f.draftID})
	req := httptest.NewRequest(http.MethodPost, httpserver.EnrollmentConfirmPath, bytes.NewReader(body))
	req.Host = "primary.example:3443"
	req.TLS = &tls.ConnectionState{Version: tls.VersionTLS13, HandshakeComplete: true, ServerName: "primary.example"}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func assertNoSecretLeak(t *testing.T, rec *httptest.ResponseRecorder, token string) {
	t.Helper()
	all := rec.Body.String() + fmt.Sprint(rec.Header())
	if strings.Contains(all, token) || strings.Contains(all, "pcenr1_") {
		t.Fatalf("response leaked the enrollment token: %s", all)
	}
}

func TestConfirmHappyPathExchangesAcceptsValidatesWithAcceptorKEKAndConverts(t *testing.T) {
	f := newConfirmFixture(t, true)
	rec := f.confirm()
	if rec.Code != http.StatusOK {
		t.Fatalf("confirm status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(f.exchangeReqs) != 1 || f.exchangeTokens[0] != f.token || f.accepts != 1 || f.validateKEKs != 1 || f.convertCalls != 1 {
		t.Fatalf("exchange=%d accepts=%d validate=%d convert=%d", len(f.exchangeReqs), f.accepts, f.validateKEKs, f.convertCalls)
	}
	req := f.exchangeReqs[0]
	if req.PrimaryInstallationID != f.envelope.Transient.SourcePrimaryID.String() || req.LeadershipGeneration != 3 ||
		req.InstallationID != f.localInstall.String() || req.NodeID != f.localNode.String() {
		t.Fatalf("binding fields = %+v", req)
	}
	if _, ok := f.cache.GetByID(f.draftID); ok {
		t.Fatal("draft must be deleted after a successful conversion")
	}
	if _, _, ok := f.cache.AttemptSecrets(f.draftID); ok {
		t.Fatal("token must be gone after success")
	}
	assertNoSecretLeak(t, rec, f.token)
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
}

func TestConfirmExchangeDeniedPersistsNothingAndIsGeneric(t *testing.T) {
	f := newConfirmFixture(t, true)
	f.exchangeErr = fmt.Errorf("primary said %s: %w", f.token, syncpublication.ErrExchangeDenied)
	rec := f.confirm()
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if f.accepts != 0 || f.convertCalls != 0 || len(f.keys) != 0 {
		t.Fatalf("accepts=%d convert=%d keys=%d", f.accepts, f.convertCalls, len(f.keys))
	}
	assertNoSecretLeak(t, rec, f.token)
	if _, _, ok := f.cache.AttemptSecrets(f.draftID); ok {
		t.Fatal("a denied token is dead and must be dropped from memory")
	}
}

func TestConfirmRateLimitedMapsTo429WithRetryAfter(t *testing.T) {
	f := newConfirmFixture(t, true)
	f.exchangeErr = &syncpublication.ExchangeRateLimitedError{RetryAfter: 42 * time.Second}
	rec := f.confirm()
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "42" {
		t.Fatalf("status=%d retry-after=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if f.accepts != 0 || f.convertCalls != 0 {
		t.Fatal("rate limited must not accept or convert")
	}
	if _, _, ok := f.cache.AttemptSecrets(f.draftID); !ok {
		t.Fatal("rate limiting is transient; the draft must stay retryable")
	}
}

func TestConfirmUnavailableMapsTo503(t *testing.T) {
	f := newConfirmFixture(t, true)
	f.exchangeErr = syncpublication.ErrExchangeUnavailable
	if rec := f.confirm(); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestConfirmRetryAfterTransientPersistenceFailureReusesTheSameGrant(t *testing.T) {
	f := newConfirmFixture(t, true)
	f.acceptErrs = []error{enrollment.ErrBootstrapPersistence}
	if rec := f.confirm(); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("first status=%d", rec.Code)
	}
	if f.convertCalls != 0 {
		t.Fatal("must not convert after a failed accept")
	}
	if rec := f.confirm(); rec.Code != http.StatusOK {
		t.Fatalf("retry status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(f.exchangeReqs) != 2 {
		t.Fatalf("exchange calls = %d", len(f.exchangeReqs))
	}
	a, b := f.exchangeReqs[0], f.exchangeReqs[1]
	if a.AttemptID != b.AttemptID || a.PreviewDigest != b.PreviewDigest || string(a.RecipientPublicKey) != string(b.RecipientPublicKey) {
		t.Fatal("retry must re-request the exact same grant (attempt, digest, recipient key)")
	}
	if len(f.sealedByAttempt) != 1 || f.convertCalls != 1 {
		t.Fatalf("grants=%d convert=%d", len(f.sealedByAttempt), f.convertCalls)
	}
}

func TestConfirmConversionFailureKeepsDraftForRetry(t *testing.T) {
	f := newConfirmFixture(t, true)
	f.convertErr = enrollment.ErrNodeConversionAborted
	if rec := f.confirm(); rec.Code != http.StatusConflict {
		t.Fatalf("status=%d", rec.Code)
	}
	if _, ok := f.cache.GetByID(f.draftID); !ok {
		t.Fatal("draft must survive a failed conversion so confirm can be retried")
	}
}

func TestConfirmWithoutRetainedTokenFailsClosed(t *testing.T) {
	f := newConfirmFixture(t, false)
	rec := f.confirm()
	if rec.Code != http.StatusGone || len(f.exchangeReqs) != 0 || f.convertCalls != 0 {
		t.Fatalf("status=%d exchanges=%d convert=%d", rec.Code, len(f.exchangeReqs), f.convertCalls)
	}
}

func TestConfirmKEKReadBackFailureAfterAcceptDoesNotConvert(t *testing.T) {
	f := newConfirmFixture(t, true)
	f.loadErr = errors.New("read-back failed")
	rec := f.confirm()
	if rec.Code == http.StatusOK || f.convertCalls != 0 || f.accepts != 1 {
		t.Fatalf("status=%d convert=%d accepts=%d", rec.Code, f.convertCalls, f.accepts)
	}
	if _, ok := f.cache.GetByID(f.draftID); !ok {
		t.Fatal("draft must stay so confirm can be retried")
	}
}

func TestConfirmRoleEligibilityIsCheckedPerRequestBeforeAnyExchange(t *testing.T) {
	key := uuid.NewString()
	for _, test := range []struct {
		name string
		role domain.TopologyRole
		key  string
		want int
	}{
		{"standalone", domain.TopologyRoleStandalone, "", http.StatusOK},
		{"primary", domain.TopologyRolePrimary, key, http.StatusOK},
		{"stale primary", domain.TopologyRoleStalePrimary, key, http.StatusOK},
		{"demoted unbound node", domain.TopologyRoleNode, "", http.StatusOK},
		{"bound node", domain.TopologyRoleNode, key, http.StatusConflict},
		{"primary with nodes", domain.TopologyRolePrimaryWithNodes, key, http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newConfirmFixture(t, true)
			f.localRole, f.localKeyID = test.role, test.key
			rec := f.confirm()
			if rec.Code != test.want {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if test.want != http.StatusOK {
				if len(f.exchangeReqs) != 0 || f.accepts != 0 || f.convertCalls != 0 {
					t.Fatalf("ineligible identity reached exchange=%d accepts=%d convert=%d", len(f.exchangeReqs), f.accepts, f.convertCalls)
				}
				if !strings.Contains(rec.Body.String(), "enrollment conversion denied") {
					t.Fatalf("expected generic denial, got %s", rec.Body.String())
				}
				assertNoSecretLeak(t, rec, f.token)
			}
		})
	}
}

func TestConfirmPassesAcceptedClusterKeyToConverter(t *testing.T) {
	f := newConfirmFixture(t, true)
	if rec := f.confirm(); rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(f.convertKeyIDs) != 1 || f.convertKeyIDs[0] == nil {
		t.Fatalf("converter key ids=%v", f.convertKeyIDs)
	}
	if _, ok := f.keys[f.convertKeyIDs[0].String()]; !ok {
		t.Fatalf("converter received %s, not the accepted key", f.convertKeyIDs[0])
	}
}

func TestConfirmRecordsSyncBindingForTheContinuousSyncLoop(t *testing.T) {
	f := newConfirmFixture(t, true)
	rec := f.confirm()
	if rec.Code != http.StatusOK {
		t.Fatalf("confirm status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(f.bindings) != 1 {
		t.Fatalf("bindings=%d, want 1", len(f.bindings))
	}
	binding := f.bindings[0]
	if !strings.HasPrefix(binding.PrimaryURL, "https://primary.example:3443") ||
		binding.PrimaryInstallationID != f.envelope.Transient.SourcePrimaryID.String() ||
		binding.CredentialID == "" || f.boundHashes[0] != f.envelope.ContentHash {
		t.Fatalf("binding=%+v hash=%q", binding, f.boundHashes[0])
	}
}

func TestConfirmFailsClosedWhenSyncBindingCannotBeRecorded(t *testing.T) {
	f := newConfirmFixture(t, true)
	f.bindErr = errors.New("disk full")
	rec := f.confirm()
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if f.convertCalls != 0 {
		t.Fatalf("convert ran %d times without a sync binding", f.convertCalls)
	}
}

func TestConfirmUnlinkedNodeMayReEnrollButTheExceptionIsNodeOnly(t *testing.T) {
	key := uuid.NewString()
	for _, test := range []struct {
		name     string
		role     domain.TopologyRole
		eligible bool
		want     int
	}{
		{"unlinked node", domain.TopologyRoleNode, true, http.StatusOK},
		{"linked node", domain.TopologyRoleNode, false, http.StatusConflict},
		{"primary with nodes never gets the exception", domain.TopologyRolePrimaryWithNodes, true, http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newConfirmFixture(t, true)
			f.localRole, f.localKeyID, f.reEnrollEligible = test.role, key, test.eligible
			rec := f.confirm()
			if rec.Code != test.want {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if test.want != http.StatusOK && (len(f.exchangeReqs) != 0 || f.convertCalls != 0) {
				t.Fatalf("refused identity reached exchange=%d convert=%d", len(f.exchangeReqs), f.convertCalls)
			}
		})
	}
}
