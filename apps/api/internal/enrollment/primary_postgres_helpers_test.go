package enrollment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	nodesync "github.com/zerkc/ProxyCore/apps/api/internal/sync"
)

type postgresPrimaryFixture struct {
	pool        *pgxpool.Pool
	store       *configuration.PgPhase2Store
	tokenStore  *Service
	service     *PrimaryService
	owner       auth.User
	now         time.Time
	masterKey   string
	request     PrimaryGrantRequest
	recipient   BootstrapRecipient
	issuerMu    sync.Mutex
	issuerCalls int
}

func newPostgresPrimaryFixture(t *testing.T, grantTTL time.Duration) *postgresPrimaryFixture {
	t.Helper()
	pool := postgresPool(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 2, 3, 4, 5, 0, time.UTC)
	owner := owner()
	if _, err := pool.Exec(ctx, `
		insert into users (id, username, password_hash, role)
		values ($1, $2, 'test-hash', 'owner')
	`, owner.ID, "owner_"+owner.ID[:8]); err != nil {
		t.Fatalf("insert owner: %v", err)
	}
	primaryID := uuidString(t)
	primaryNodeID := uuidString(t)
	if _, err := pool.Exec(ctx, `
		insert into installation_identity (
			id, installation_id, node_id, role, leadership_generation, latest_known_generation
		) values ('default', $1, $2, 'standalone-primary', 1, 1)
	`, primaryID, primaryNodeID); err != nil {
		t.Fatalf("insert primary identity: %v", err)
	}
	masterKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x19}, 32))
	store := configuration.NewPhase2StoreWithMasterKey(pool, masterKey)
	tokenStore := NewService(store, Options{TTL: 10 * time.Minute, Now: func() time.Time { return now }})
	created, err := tokenStore.CreateToken(ctx, owner)
	if err != nil {
		t.Fatalf("create enrollment token: %v", err)
	}
	attemptID := uuidString(t)
	targetInstallationID := uuidString(t)
	targetNodeID := uuidString(t)
	previewDigest := bytes.Repeat([]byte{0x29}, sha256.Size)
	recipient, err := NewBootstrapRecipient()
	if err != nil {
		t.Fatalf("new bootstrap recipient: %v", err)
	}
	t.Cleanup(recipient.Destroy)
	publicKey, err := recipient.PublicKey()
	if err != nil {
		t.Fatalf("recipient public key: %v", err)
	}
	fixture := &postgresPrimaryFixture{
		pool: pool, store: store, tokenStore: tokenStore, owner: owner, now: now,
		masterKey: masterKey, recipient: recipient,
		request: PrimaryGrantRequest{
			EnrollmentToken: created.Token, AttemptID: attemptID,
			TargetInstallationID: targetInstallationID, TargetNodeID: targetNodeID,
			RecipientPublicKey: publicKey, VerifiedPreviewDigest: previewDigest,
			ExpectedPrimaryInstallationID: primaryID, ExpectedLeadershipGeneration: 1,
			GrantTTL: grantTTL,
		},
	}
	fixture.service = NewPrimaryService(store, PrimaryServiceOptions{
		Now: func() time.Time { return now },
		CredentialIssuer: testNodeCredentialIssuer(func(context.Context) (NodeCredentialMaterial, error) {
			fixture.issuerMu.Lock()
			fixture.issuerCalls++
			fixture.issuerMu.Unlock()
			credential, err := nodesync.GenerateNodeCredential()
			if err != nil {
				return NodeCredentialMaterial{}, err
			}
			material := NodeCredentialMaterial{
				ID: credential.ID(), Secret: credential.CopyBytes(),
				Hash: credential.Hash(), HashVersion: nodesync.NodeCredentialHashVersion,
			}
			credential.Destroy()
			return material, nil
		}),
	})
	return fixture
}

type postgresGrantRow struct {
	installationID string
	nodeID         string
	primaryID      string
	generation     int64
	recipient      string
	previewDigest  string
	payload        string
	payloadHash    string
}

func readPostgresGrant(t *testing.T, pool *pgxpool.Pool, attemptID string) postgresGrantRow {
	t.Helper()
	var row postgresGrantRow
	if err := pool.QueryRow(context.Background(), `
		select installation_id::text, node_id::text, primary_id::text, primary_generation,
			node_ephemeral_public_key, verified_preview_digest, sealed_bootstrap_payload, payload_hash
		from enrollment_grants where attempt_id = $1
	`, attemptID).Scan(&row.installationID, &row.nodeID, &row.primaryID, &row.generation,
		&row.recipient, &row.previewDigest, &row.payload, &row.payloadHash); err != nil {
		t.Fatalf("read enrollment grant: %v", err)
	}
	return row
}

func assertPostgresGrantSecretsAbsent(t *testing.T, pool *pgxpool.Pool, values ...string) {
	t.Helper()
	var tokenHash, wrappedKey, credentialHash, payload string
	if err := pool.QueryRow(context.Background(), `
		select
			(select token_hash from enrollment_tokens limit 1),
			(select wrapped_kek from cluster_keys limit 1),
			(select credential_hash from node_credentials limit 1),
			(select sealed_bootstrap_payload from enrollment_grants limit 1)
	`).Scan(&tokenHash, &wrappedKey, &credentialHash, &payload); err != nil {
		t.Fatalf("read persisted secret surfaces: %v", err)
	}
	for _, value := range values {
		if value == "" {
			continue
		}
		for name, stored := range map[string]string{
			"token hash": tokenHash, "wrapped key": wrappedKey,
			"credential hash": credentialHash, "sealed payload": payload,
		} {
			if strings.Contains(stored, value) {
				t.Fatalf("%s contains plaintext secret", name)
			}
		}
	}
}

func findBearerForPersistedCredential(t *testing.T, pool *pgxpool.Pool, credentialID string, secret []byte) string {
	t.Helper()
	defer zeroBytes(secret)
	var nodeID string
	if err := pool.QueryRow(context.Background(), `select node_id::text from node_credentials where id = $1`, credentialID).Scan(&nodeID); err != nil {
		t.Fatalf("find credential node: %v", err)
	}
	credential, err := nodesync.NewNodeCredential(credentialID, secret)
	if err != nil {
		t.Fatalf("rebuild credential bearer: %v", err)
	}
	defer credential.Destroy()
	return credential.BearerCopy()
}

func assertCount(t *testing.T, pool *pgxpool.Pool, table string, want int) {
	t.Helper()
	var got int
	if err := pool.QueryRow(context.Background(), "select count(*) from "+table).Scan(&got); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if got != want {
		t.Fatalf("count %s = %d, want %d", table, got, want)
	}
}

func uuidString(t *testing.T) string {
	t.Helper()
	return uuid.NewString()
}

func hexDigest(value []byte) string { return fmt.Sprintf("%x", value) }

func mustWire(t *testing.T, key BootstrapRecipientPublicKey) []byte {
	t.Helper()
	wire, err := key.Wire()
	if err != nil {
		t.Fatalf("recipient wire: %v", err)
	}
	return wire
}

func tokenSelector(t *testing.T, token string) string {
	t.Helper()
	selector, _, ok := parseToken(token)
	if !ok {
		t.Fatal("invalid test token")
	}
	return selector
}

func tokenID(t *testing.T, token string, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `select id::text from enrollment_tokens where token_selector = $1`, tokenSelector(t, token)).Scan(&id); err != nil {
		t.Fatalf("read token id: %v", err)
	}
	return id
}
