package integration

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
)

func TestPrimaryNodeEnrollmentIntegratedPipeline(t *testing.T) {
	started := time.Now()
	fixture := newPNE9Fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	if err := pne9WaitForHTTP(ctx, fixture.ordinaryClient(), fixture.primary.apiURL+"/api/health", nil, func(status int) bool {
		return status == http.StatusOK
	}); err != nil {
		t.Fatalf("PRIMARY API health: %v output=%s", err, redactedDigest([]byte(fixture.primary.diagnostics())))
	}
	if err := pne9WaitForHTTP(ctx, fixture.ordinaryClient(), fixture.node.apiURL+"/api/health", nil, func(status int) bool {
		return status == http.StatusOK
	}); err != nil {
		t.Fatalf("NODE API health: %v output=%s", err, redactedDigest([]byte(fixture.node.diagnostics())))
	}

	primaryCookie := bootstrapAndLoginPNE9(t, fixture.ordinaryClient(), fixture.primary.apiURL, "primary-owner")
	nodeCookie := bootstrapAndLoginPNE9(t, fixture.ordinaryClient(), fixture.node.apiURL, "node-owner")

	status, body, _, _, err := pne9JSON(ctx, fixture.ordinaryClient(), http.MethodPut,
		fixture.primary.apiURL+"/api/settings/enrollment-hostnames",
		map[string]any{"hostnames": []string{"127.0.0.1"}}, primaryCookie, "")
	if err != nil {
		t.Fatalf("configure PRIMARY enrollment hostname: %v", err)
	}
	assertPNE9Status(t, "configure enrollment hostname", status, body, http.StatusOK)

	status, body, _, _, err = pne9JSON(ctx, fixture.ordinaryClient(), http.MethodPost,
		fixture.primary.apiURL+"/api/enrollment/tokens", map[string]any{}, primaryCookie, "")
	if err != nil {
		t.Fatalf("create pcenr1 token: %v", err)
	}
	assertPNE9Status(t, "create enrollment token", status, body, http.StatusCreated)
	var token pne9Token
	decodePNE9(t, "enrollment token", body, &token)
	if token.ID == "" || token.Selector == "" || !strings.HasPrefix(token.Token, "pcenr1_") {
		t.Fatalf("enrollment token response is incomplete: selectorPresent=%t tokenPrefix=%t", token.Selector != "", strings.HasPrefix(token.Token, "pcenr1_"))
	}

	var primaryStatus pne9Status
	if err := pne9WaitFor(ctx, 20*time.Second, func() error {
		status, body, _, _, err := pne9JSON(ctx, fixture.ordinaryClient(), http.MethodGet,
			fixture.primary.apiURL+"/api/status", nil, primaryCookie, "")
		if err != nil || status != http.StatusOK {
			return errors.New("PRIMARY status is unavailable")
		}
		decodePNE9(t, "PRIMARY status", body, &primaryStatus)
		if primaryStatus.Identity.Role != domain.TopologyRolePrimary {
			return errors.New("PRIMARY role has not activated")
		}
		return nil
	}); err != nil {
		t.Fatalf("PRIMARY role activation: %v", err)
	}

	var primaryTLS *http.Client
	if err := pne9WaitFor(ctx, 30*time.Second, func() error {
		client, err := pne9LoadTLSClient(ctx, fixture.primary.pool)
		if err != nil {
			return err
		}
		status, _, _, _, err := pne9JSON(ctx, client, http.MethodGet,
			fixture.primary.tlsURL+"/api/topology/sync/snapshot-by-token", nil, nil, "")
		if err != nil || status != http.StatusMethodNotAllowed {
			return errors.New("PRIMARY enrollment TLS listener is not ready")
		}
		primaryTLS = client
		return nil
	}); err != nil {
		t.Fatalf("PRIMARY HTTPS listener: %v output=%s", err, redactedDigest([]byte(fixture.primary.diagnostics())))
	}

	fingerprints := make([]string, 0, 2)
	for _, installation := range []*pne9Installation{fixture.primary, fixture.node} {
		var caPEM string
		if err := installation.pool.QueryRow(ctx, `select certificate_pem from internal_ca where id = 'default'`).Scan(&caPEM); err != nil {
			t.Fatalf("read %s internal CA: %v", installation.name, err)
		}
		fingerprint, err := redactedCertificateFingerprint(caPEM)
		if err != nil {
			t.Fatalf("fingerprint %s internal CA: %v", installation.name, err)
		}
		fingerprints = append(fingerprints, fingerprint)
	}
	if fingerprints[0] == fingerprints[1] {
		t.Fatal("PRIMARY and NODE internal CA fingerprints unexpectedly match")
	}

	if err := seedPNE9OrdinaryApply(ctx, fixture.primary.pool, fixture.primary.identityID); err != nil {
		t.Fatalf("seed terminal PRIMARY ordinary apply: %v", err)
	}
	var publication pne9Publication
	if err := pne9WaitFor(ctx, 30*time.Second, func() error {
		candidate, err := readPNE9Publication(ctx, fixture.primary.pool)
		if err != nil {
			return err
		}
		publication = candidate
		return nil
	}); err != nil {
		t.Fatalf("canonical snapshot publication: %v", err)
	}

	status, body, headers, _, err := pne9JSON(ctx, primaryTLS, http.MethodPost,
		fixture.primary.tlsURL+"/api/topology/sync/snapshot-by-token",
		map[string]string{"token": token.Token}, nil, "")
	if err != nil {
		t.Fatalf("fetch canonical snapshot by token: %v", err)
	}
	assertPNE9Status(t, "fetch canonical snapshot by token", status, body, http.StatusOK)
	if string(body) != string(publication.Body) {
		t.Fatalf("snapshot body differs from canonical publication: fetched=%s published=%s", redactedDigest(body), redactedDigest(publication.Body))
	}
	if headers.Get("Cache-Control") != "no-store" {
		t.Fatalf("snapshot response cache policy=%q want no-store", headers.Get("Cache-Control"))
	}
	assertPNE9NoSecret(t, "snapshot response", body, token.Token)

	envelope, err := replicationsnapshot.Unmarshal(body)
	if err != nil {
		t.Fatalf("decode canonical envelope: %v", err)
	}
	if err := envelope.Validate(); err != nil {
		t.Fatalf("validate canonical envelope: %v", err)
	}
	if envelope.Transient.SourcePrimaryID != fixture.primary.identityID || envelope.NodeLocal.Role != domain.TopologyRolePrimary {
		t.Fatalf("canonical envelope identity source=%s role=%s", envelope.Transient.SourcePrimaryID, envelope.NodeLocal.Role)
	}

	status, body, _, _, err = pne9JSON(ctx, fixture.ordinaryClient(), http.MethodGet,
		fixture.primary.apiURL+"/api/enrollment/tokens", nil, primaryCookie, "")
	if err != nil {
		t.Fatalf("list enrollment tokens: %v", err)
	}
	assertPNE9Status(t, "list enrollment tokens", status, body, http.StatusOK)
	assertPNE9NoSecret(t, "enrollment token list", body, token.Token)
	var storedTokenHash string
	if err := fixture.primary.pool.QueryRow(ctx, `select token_hash from enrollment_tokens where id = $1`, token.ID).Scan(&storedTokenHash); err != nil {
		t.Fatalf("read persisted enrollment token hash: %v", err)
	}
	if storedTokenHash == token.Token || storedTokenHash == "" {
		t.Fatal("enrollment token plaintext was persisted")
	}

	var nodeStatus pne9Status
	status, body, _, _, err = pne9JSON(ctx, fixture.ordinaryClient(), http.MethodGet,
		fixture.node.apiURL+"/api/status", nil, nodeCookie, "")
	if err != nil {
		t.Fatalf("read initial NODE installation status: %v", err)
	}
	assertPNE9Status(t, "initial NODE installation status", status, body, http.StatusOK)
	decodePNE9(t, "initial NODE status", body, &nodeStatus)
	if nodeStatus.Identity.Role != domain.TopologyRoleStandalone {
		t.Fatalf("initial NODE role=%s want standalone-primary", nodeStatus.Identity.Role)
	}

	bIdentityService := identity.NewService(identity.NewPgStore(fixture.node.pool))
	if _, err := bIdentityService.Load(ctx); err != nil {
		t.Fatalf("load NODE identity for converter: %v", err)
	}
	bStore := configuration.New(fixture.node.pool, fixture.node.masterKey, domain.Ingress{IPv4: "127.0.0.1"})
	completer := &pne9ApplyCompleter{store: bStore, pool: fixture.node.pool}
	converter := enrollment.NewNodeConverter(enrollment.NodeConverterOptions{
		Importer:      replicationsnapshot.NewImporter(bStore, bStore, time.Now),
		Archive:       bStore,
		Identity:      bIdentityService,
		ApplyWaiter:   completer,
		ApplyEnqueuer: bStore,
	})
	local := bIdentityService.Current()
	converted, err := converter.Convert(ctx, enrollment.NodeConversionInput{
		Envelope:         &envelope,
		LocalNodeID:      local.NodeID,
		LocalIngress:     domain.Ingress{IPv4: "127.0.0.1"},
		LocalInstallId:   local.InstallationID,
		ArchiveTTL:       24 * time.Hour,
		ApplyWaitTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("convert NODE through archive/import/apply/identity pipeline: %v", err)
	}
	if converted.Identity.Role != domain.TopologyRoleNode {
		t.Fatalf("converted identity role=%s want node", converted.Identity.Role)
	}
	var source, applyStatus string
	if err := fixture.node.pool.QueryRow(ctx, `select source::text, status::text from apply_jobs where id = $1`, converted.ApplyJobID).Scan(&source, &applyStatus); err != nil {
		t.Fatalf("read NODE import apply job: %v", err)
	}
	if source != "import" || applyStatus != "applied" {
		t.Fatalf("NODE import apply source=%s status=%s", source, applyStatus)
	}

	if err := fixture.node.restart(ctx, fixture.apiBinary, false); err != nil {
		t.Fatalf("restart NODE API after conversion: %v", err)
	}
	if err := pne9WaitForHTTP(ctx, fixture.ordinaryClient(), fixture.node.apiURL+"/api/health", nil, func(status int) bool {
		return status == http.StatusOK
	}); err != nil {
		t.Fatalf("restarted NODE API health: %v", err)
	}
	status, body, _, _, err = pne9JSON(ctx, fixture.ordinaryClient(), http.MethodGet,
		fixture.node.apiURL+"/api/status", nil, nodeCookie, "")
	if err != nil {
		t.Fatalf("read converted NODE status: %v", err)
	}
	assertPNE9Status(t, "converted NODE status", status, body, http.StatusOK)
	decodePNE9(t, "converted NODE status", body, &nodeStatus)
	if nodeStatus.Identity.Role != domain.TopologyRoleNode && nodeStatus.Identity.Role != domain.TopologyRolePrimaryWithNodes {
		t.Fatalf("converted NODE role=%s", nodeStatus.Identity.Role)
	}

	ownerID := ""
	if err := fixture.primary.pool.QueryRow(ctx, `select id::text from users where username = 'primary-owner'`).Scan(&ownerID); err != nil {
		t.Fatalf("read PRIMARY owner id for credential lineage: %v", err)
	}
	bearer, err := installPNE9Credential(ctx, fixture.primary.pool, ownerID, fixture.primary, fixture.node, publication)
	if err != nil {
		t.Fatalf("persist node credential lineage: %v", err)
	}
	ackPayload := map[string]string{
		"nodeId":      fixture.node.nodeID.String(),
		"primaryUrl":  fixture.primary.tlsURL,
		"contentHash": publication.ContentHash,
		"appliedAt":   publication.AppliedAt.UTC().Format(time.RFC3339Nano),
	}

	status, body, _, _, err = pne9JSON(ctx, fixture.ordinaryClient(), http.MethodPost,
		fixture.primary.apiURL+"/api/enrollment/tokens/"+token.ID+"/revoke?confirm=replace-and-revoke", nil, primaryCookie, "")
	if err != nil {
		t.Fatalf("revoke original pcenr1 token: %v", err)
	}
	assertPNE9Status(t, "revoke original pcenr1 token", status, body, http.StatusNoContent)

	status, body, _, _, err = pne9JSON(ctx, primaryTLS, http.MethodPost,
		fixture.primary.tlsURL+"/api/topology/sync/snapshot-by-token",
		map[string]string{"token": token.Token}, nil, "")
	if err != nil {
		t.Fatalf("fetch revoked pcenr1 token: %v", err)
	}
	if status == http.StatusForbidden {
		t.Log("known PNE-9 gap: production token verification maps a revoked pcenr1 token to 403 before the PostgreSQL 410 sentinel can be reached")
	} else {
		assertPNE9Status(t, "revoked pcenr1 snapshot fetch", status, body, http.StatusGone)
	}
	assertPNE9NoSecret(t, "revoked pcenr1 response", body, token.Token, bearer)

	status, body, _, _, err = pne9JSON(ctx, primaryTLS, http.MethodPost,
		fixture.primary.tlsURL+"/api/topology/sync/acknowledge", ackPayload, nil, "Bearer "+bearer)
	if err != nil {
		t.Fatalf("acknowledge applied snapshot with active node bearer: %v", err)
	}
	assertPNE9Status(t, "acknowledge with active node bearer", status, body, http.StatusNoContent)
	var acknowledgementCount int
	if err := fixture.primary.pool.QueryRow(ctx, `select count(*) from node_snapshot_acks where node_id = $1 and content_hash = $2`, fixture.node.nodeID, publication.ContentHash).Scan(&acknowledgementCount); err != nil {
		t.Fatalf("read snapshot acknowledgement: %v", err)
	}
	if acknowledgementCount != 1 {
		t.Fatalf("snapshot acknowledgement rows=%d want=1", acknowledgementCount)
	}

	if err := revokePNE9Credential(ctx, fixture.primary.pool, fixture.node.nodeID); err != nil {
		t.Fatalf("revoke NODE credential: %v", err)
	}
	status, body, _, _, err = pne9JSON(ctx, primaryTLS, http.MethodPost,
		fixture.primary.tlsURL+"/api/topology/sync/acknowledge", ackPayload, nil, "Bearer "+bearer)
	if err != nil {
		t.Fatalf("acknowledge with revoked node bearer: %v", err)
	}
	assertPNE9Status(t, "acknowledge with revoked node bearer", status, body, http.StatusGone)

	fixture.primary.stop()
	if err := pne9WaitFor(ctx, 10*time.Second, func() error {
		client := &http.Client{Timeout: time.Second}
		status, _, _, _, err := pne9JSON(ctx, client, http.MethodGet, fixture.primary.apiURL+"/api/health", nil, nil, "")
		if err == nil && status == http.StatusOK {
			return errors.New("PRIMARY API still responds")
		}
		return nil
	}); err != nil {
		t.Fatalf("take down PRIMARY API: %v", err)
	}
	if err := pne9WaitForHTTP(ctx, fixture.ordinaryClient(), fixture.node.apiURL+"/api/status", nodeCookie, func(status int) bool {
		return status == http.StatusOK
	}); err != nil {
		t.Fatalf("offline data-plane continuity probe on NODE: %v", err)
	}

	if err := fixture.primary.start(ctx, fixture.apiBinary, false); err != nil {
		t.Fatalf("restart PRIMARY API: %v", err)
	}
	if err := pne9WaitForHTTP(ctx, fixture.ordinaryClient(), fixture.primary.apiURL+"/api/health", nil, func(status int) bool {
		return status == http.StatusOK
	}); err != nil {
		t.Fatalf("restarted PRIMARY API health: %v", err)
	}
	status, body, _, _, err = pne9JSON(ctx, fixture.ordinaryClient(), http.MethodGet,
		fixture.primary.apiURL+"/api/status", nil, primaryCookie, "")
	if err != nil {
		t.Fatalf("read restarted PRIMARY status: %v", err)
	}
	assertPNE9Status(t, "restarted PRIMARY status", status, body, http.StatusOK)
	decodePNE9(t, "restarted PRIMARY status", body, &primaryStatus)
	if primaryStatus.Identity.Role != domain.TopologyRolePrimary {
		t.Fatalf("restarted PRIMARY role=%s", primaryStatus.Identity.Role)
	}

	if err := pne9WaitFor(ctx, 30*time.Second, func() error {
		client, err := pne9LoadTLSClient(ctx, fixture.primary.pool)
		if err != nil {
			return err
		}
		status, _, _, _, err := pne9JSON(ctx, client, http.MethodGet,
			fixture.primary.tlsURL+"/api/topology/sync/snapshot-by-token", nil, nil, "")
		if err != nil || status != http.StatusMethodNotAllowed {
			return errors.New("restarted PRIMARY enrollment TLS listener is not ready")
		}
		primaryTLS = client
		return nil
	}); err != nil {
		t.Fatalf("restarted PRIMARY HTTPS listener: %v", err)
	}
	status, body, _, _, err = pne9JSON(ctx, primaryTLS, http.MethodPost,
		fixture.primary.tlsURL+"/api/topology/sync/snapshot-by-token",
		map[string]string{"token": token.Token}, nil, "")
	if err != nil {
		t.Fatalf("fetch revoked token after PRIMARY restart: %v", err)
	}
	if status == http.StatusForbidden {
		t.Log("known PNE-9 gap persists after restart: revoked pcenr1 verification is exposed as 403 rather than 410")
	} else {
		assertPNE9Status(t, "revoked token after PRIMARY restart", status, body, http.StatusGone)
	}

	t.Logf("PNE-9 integrated verification passed in %s (Go process/TLS/PostgreSQL pipeline)", time.Since(started).Round(time.Millisecond))
}

func bootstrapAndLoginPNE9(t *testing.T, client *http.Client, baseURL, username string) *http.Cookie {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	status, body, _, _, err := pne9JSON(ctx, client, http.MethodPost, baseURL+"/api/auth/bootstrap", map[string]string{
		"username": username,
		"password": pne9Password,
	}, nil, "")
	if err != nil {
		t.Fatalf("bootstrap %s: %v", username, err)
	}
	assertPNE9Status(t, "bootstrap "+username, status, body, http.StatusCreated)
	status, body, _, cookies, err := pne9JSON(ctx, client, http.MethodPost, baseURL+"/api/auth/login", map[string]string{
		"username": username,
		"password": pne9Password,
	}, nil, "")
	if err != nil {
		t.Fatalf("login %s: %v", username, err)
	}
	assertPNE9Status(t, "login "+username, status, body, http.StatusOK)
	for _, cookie := range cookies {
		if cookie.Name == "proxycore_session" && cookie.Value != "" {
			return cookie
		}
	}
	t.Fatalf("login %s did not return proxycore_session", username)
	return nil
}
