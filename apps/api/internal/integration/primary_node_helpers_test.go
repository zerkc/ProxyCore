package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/cluster"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
	syncpublication "github.com/zerkc/ProxyCore/apps/api/internal/sync"
)

const (
	pne9Password     = "correct horse battery staple"
	pne9PollInterval = 250 * time.Millisecond
	pne9HTTPTimeout  = 5 * time.Second
)

type pne9Installation struct {
	name       string
	schema     string
	database   string
	masterKey  string
	apiAddr    string
	tlsAddr    string
	apiURL     string
	tlsURL     string
	pool       *pgxpool.Pool
	identityID uuid.UUID
	nodeID     uuid.UUID
	clusterKey uuid.UUID
	process    *pne9Process
}

type pne9Output struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (o *pne9Output) Write(value []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buffer.Write(value)
}

func (o *pne9Output) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buffer.String()
}

type pne9Process struct {
	cmd    *exec.Cmd
	done   chan error
	output *pne9Output
}

type pne9Fixture struct {
	root       string
	apiBinary  string
	admin      *pgxpool.Pool
	primary    *pne9Installation
	node       *pne9Installation
	httpClient *http.Client
}

type pne9Token struct {
	ID        string    `json:"id"`
	Selector  string    `json:"selector"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type pne9Identity struct {
	InstallationID        string              `json:"installationId"`
	NodeID                string              `json:"nodeId"`
	Role                  domain.TopologyRole `json:"role"`
	LeadershipGeneration  int64               `json:"leadershipGeneration"`
	LatestKnownGeneration int64               `json:"latestKnownGeneration"`
	ClusterKeyID          string              `json:"clusterKeyId"`
}

type pne9Status struct {
	Identity pne9Identity `json:"identity"`
}

type pne9Publication struct {
	Body           []byte
	ContentHash    string
	SnapshotID     string
	RevisionID     string
	ApplyJobID     string
	SnapshotVer    int
	ReplicationVer int
	Generation     int64
	AppliedAt      time.Time
}

func newPNE9Fixture(t *testing.T) *pne9Fixture {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping PNE-9 process integration in short mode")
	}
	baseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if baseURL == "" {
		t.Skip("DATABASE_URL is not set; PNE-9 requires real PostgreSQL")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain is unavailable; skipping PNE-9 process integration")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, baseURL)
	if err != nil {
		t.Skipf("cannot connect to DATABASE_URL: %v", err)
	}
	f := &pne9Fixture{admin: admin}
	t.Cleanup(func() { f.close() })

	root, apiDir := pne9ProjectPaths()
	f.root = root
	binaryDir := t.TempDir()
	f.apiBinary = filepath.Join(binaryDir, "proxycore-api")
	build := exec.CommandContext(ctx, "go", "build", "-o", f.apiBinary, "./cmd/server")
	build.Dir = apiDir
	if output, err := build.CombinedOutput(); err != nil {
		admin.Close()
		t.Fatalf("build API binary: %v (%s)", err, redactedDigest(output))
	}

	f.primary = preparePNE9Installation(t, ctx, admin, baseURL, "primary", "127.0.0.1:13443", "127.0.0.1:13444", randomMasterKey(t))
	f.node = preparePNE9Installation(t, ctx, admin, baseURL, "node", "127.0.0.1:23443", "127.0.0.1:23444", randomMasterKey(t))
	f.httpClient = &http.Client{
		Timeout: pne9HTTPTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	if err := f.primary.start(ctx, f.apiBinary, false); err != nil {
		t.Fatalf("start PRIMARY API: %v", err)
	}
	if err := f.node.start(ctx, f.apiBinary, true); err != nil {
		t.Fatalf("start NODE API: %v", err)
	}
	return f
}

func preparePNE9Installation(t *testing.T, ctx context.Context, admin *pgxpool.Pool, baseURL, name, apiAddr, tlsAddr, masterKey string) *pne9Installation {
	t.Helper()
	schema := "proxycore_pne9_" + name + "_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ident := (pgx.Identifier{schema}).Sanitize()
	if _, err := admin.Exec(ctx, "create schema "+ident); err != nil {
		t.Fatalf("create %s schema: %v", name, err)
	}
	database, err := pne9SchemaURL(baseURL, schema)
	if err != nil {
		t.Fatalf("build %s database URL: %v", name, err)
	}
	pool, err := pgxpool.New(ctx, database)
	if err != nil {
		t.Fatalf("connect %s schema: %v", name, err)
	}
	if err := auth.NewPostgresStore(pool).EnsureSchema(ctx); err != nil {
		pool.Close()
		t.Fatalf("ensure %s auth schema: %v", name, err)
	}
	if err := configuration.EnsureSchema(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("ensure %s configuration schema: %v", name, err)
	}

	keyStore := cluster.NewStore(masterKey)
	tx, err := pool.Begin(ctx)
	if err != nil {
		pool.Close()
		t.Fatalf("begin %s cluster key setup: %v", name, err)
	}
	material, err := keyStore.LoadOrCreate(ctx, tx, nil)
	if err != nil {
		_ = tx.Rollback(ctx)
		pool.Close()
		t.Fatalf("create %s cluster key: %v", name, err)
	}
	clusterKeyID := material.ID
	material.Destroy()
	if err := tx.Commit(ctx); err != nil {
		pool.Close()
		t.Fatalf("commit %s cluster key: %v", name, err)
	}

	installationID := uuid.New()
	nodeID := uuid.New()
	if _, err := pool.Exec(ctx, `
		insert into installation_identity (
			id, installation_id, node_id, role, leadership_generation,
			latest_known_generation, cluster_key_id
		) values ('default', $1, $2, 'standalone-primary', 1, 1, $3)
	`, installationID, nodeID, clusterKeyID); err != nil {
		pool.Close()
		t.Fatalf("insert %s identity: %v", name, err)
	}
	// Generate an independent internal CA for each installation before the
	// enrollment leaf is issued. The production code currently obtains entropy
	// internally; INTERNAL_CA_SEED is still set on the child process so a future
	// deterministic provider cannot accidentally make the installations share CA
	// material.
	if _, err := configuration.New(pool, masterKey, domain.Ingress{}).EnsureInternalCACertificatePEM(ctx); err != nil {
		pool.Close()
		t.Fatalf("create %s internal CA: %v", name, err)
	}
	return &pne9Installation{
		name: name, schema: schema, database: database, masterKey: masterKey,
		apiAddr: apiAddr, tlsAddr: tlsAddr,
		apiURL: "http://" + apiAddr, tlsURL: "https://" + tlsAddr,
		pool: pool, identityID: installationID, nodeID: nodeID, clusterKey: clusterKeyID,
	}
}

func (f *pne9Fixture) close() {
	if f == nil {
		return
	}
	if f.primary != nil {
		f.primary.stop()
	}
	if f.node != nil {
		f.node.stop()
	}
	if f.primary != nil && f.primary.pool != nil {
		f.primary.pool.Close()
	}
	if f.node != nil && f.node.pool != nil {
		f.node.pool.Close()
	}
	if f.admin != nil {
		if f.primary != nil {
			_, _ = f.admin.Exec(context.Background(), "drop schema if exists "+(pgx.Identifier{f.primary.schema}).Sanitize()+" cascade")
		}
		if f.node != nil {
			_, _ = f.admin.Exec(context.Background(), "drop schema if exists "+(pgx.Identifier{f.node.schema}).Sanitize()+" cascade")
		}
		f.admin.Close()
	}
}

func (i *pne9Installation) start(ctx context.Context, binary string, enableNodeConverter bool) error {
	if i == nil || strings.TrimSpace(binary) == "" {
		return errors.New("installation process is not configured")
	}
	_, apiDir := pne9ProjectPaths()
	args := []string{}
	if enableNodeConverter {
		args = append(args, "--enable-node-converter")
	}
	cmd := exec.Command(binary, args...)
	cmd.Dir = apiDir
	seed := randomMasterKeyValue()
	cmd.Env = pne9Environment(i, seed)
	output := &pne9Output{}
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	i.process = &pne9Process{cmd: cmd, done: done, output: output}
	return nil
}

func (i *pne9Installation) restart(ctx context.Context, binary string, enableNodeConverter bool) error {
	i.stop()
	return i.start(ctx, binary, enableNodeConverter)
}

func (i *pne9Installation) diagnostics() string {
	if i == nil || i.process == nil || i.process.output == nil {
		return "no process output"
	}
	return i.process.output.String()
}

func (i *pne9Installation) stop() {
	if i == nil || i.process == nil {
		return
	}
	process := i.process
	i.process = nil
	if process.cmd.Process != nil {
		_ = process.cmd.Process.Kill()
	}
	select {
	case <-process.done:
	case <-time.After(10 * time.Second):
	}
}

func pne9Environment(i *pne9Installation, seed string) []string {
	values := map[string]string{
		"DATABASE_URL":                    i.database,
		"PROXYCORE_API_ADDR":              i.apiAddr,
		"PROXYCORE_ENROLLMENT_TLS_ADDR":   i.tlsAddr,
		"PROXYCORE_MASTER_KEY_BASE64":     i.masterKey,
		"PROXYCORE_UI_DIST":               filepath.Join(mustProjectRoot(), "apps", "ui", "dist"),
		"PROXYCORE_UPDATE_CHECK_ENABLED":  "0",
		"PROXYCORE_CERT_RENEWAL_INTERVAL": "24h",
		"PROXYCORE_SECURE_COOKIES":        "0",
		"SESSION_TTL_SECONDS":             "3600",
		"INTERNAL_CA_SEED":                seed,
	}
	return overrideEnvironment(values)
}

func overrideEnvironment(values map[string]string) []string {
	current := make(map[string]string)
	for _, value := range os.Environ() {
		key, item, ok := strings.Cut(value, "=")
		if ok {
			current[key] = item
		}
	}
	delete(current, "PGOPTIONS")
	for key, value := range values {
		current[key] = value
	}
	result := make([]string, 0, len(current))
	for key, value := range current {
		result = append(result, key+"="+value)
	}
	return result
}

func (f *pne9Fixture) ordinaryClient() *http.Client {
	if f != nil && f.httpClient != nil {
		return f.httpClient
	}
	return &http.Client{Timeout: pne9HTTPTimeout}
}

func pne9TLSClient(certificatePEM string) (*http.Client, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(certificatePEM)) {
		return nil, errors.New("internal CA certificate could not be parsed")
	}
	return &http.Client{
		Timeout: pne9HTTPTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS13,
				RootCAs:    roots,
				ServerName: "127.0.0.1",
			},
		},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

func pne9LoadTLSClient(ctx context.Context, pool *pgxpool.Pool) (*http.Client, error) {
	var certificatePEM string
	if err := pool.QueryRow(ctx, `select certificate_pem from internal_ca where id = 'default'`).Scan(&certificatePEM); err != nil {
		return nil, err
	}
	return pne9TLSClient(certificatePEM)
}

func pne9JSON(ctx context.Context, client *http.Client, method, target string, payload any, cookie *http.Cookie, authorization string) (int, []byte, http.Header, []*http.Cookie, error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, nil, nil, err
		}
		body = strings.NewReader(string(encoded))
	}
	request, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return 0, nil, nil, nil, err
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		request.AddCookie(cookie)
	}
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, nil, nil, err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return response.StatusCode, nil, response.Header, response.Cookies(), err
	}
	return response.StatusCode, responseBody, response.Header, response.Cookies(), nil
}

func pne9WaitFor(ctx context.Context, timeout time.Duration, check func() error) error {
	deadline := time.After(timeout)
	for {
		if err := check(); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return errors.New("verification condition timed out")
		case <-time.After(pne9PollInterval):
		}
	}
}

func pne9WaitForHTTP(ctx context.Context, client *http.Client, target string, cookie *http.Cookie, want func(int) bool) error {
	return pne9WaitFor(ctx, 30*time.Second, func() error {
		status, _, _, _, err := pne9JSON(ctx, client, http.MethodGet, target, nil, cookie, "")
		if err != nil || !want(status) {
			return errors.New("HTTP endpoint is not ready")
		}
		return nil
	})
}

func pne9ProjectPaths() (root, apiDir string) {
	_, file, _, _ := runtime.Caller(0)
	apiDir = filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	root = filepath.Clean(filepath.Join(apiDir, "..", ".."))
	return root, apiDir
}

func mustProjectRoot() string {
	root, _ := pne9ProjectPaths()
	return root
}

func pne9SchemaURL(baseURL, schema string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("DATABASE_URL must be a PostgreSQL URL")
	}
	query := parsed.Query()
	query.Set("options", "-c search_path="+schema)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func randomMasterKey(t *testing.T) string {
	t.Helper()
	return randomMasterKeyValue()
}

func randomMasterKeyValue() string {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		panic("generate PNE-9 master key: " + err.Error())
	}
	return base64.StdEncoding.EncodeToString(value)
}

func redactedDigest(value []byte) string {
	digest := sha256.Sum256(value)
	return fmt.Sprintf("len=%d sha256=%x", len(value), digest)
}

func redactedCertificateFingerprint(certificatePEM string) (string, error) {
	block, rest := pem.Decode([]byte(certificatePEM))
	if block == nil || block.Type != "CERTIFICATE" || len(strings.TrimSpace(string(rest))) != 0 {
		return "", errors.New("invalid certificate PEM")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(certificate.Raw)
	return fmt.Sprintf("%x", digest), nil
}

type pne9ApplyCompleter struct {
	store *configuration.Store
	pool  *pgxpool.Pool
	mu    sync.Mutex
	done  bool
}

func (c *pne9ApplyCompleter) GetApplyJobTerminal(ctx context.Context, jobID uuid.UUID) (configuration.ApplyJobTerminal, error) {
	if c == nil || c.store == nil || c.pool == nil {
		return configuration.ApplyJobTerminal{}, errors.New("apply completer unavailable")
	}
	current, err := c.store.GetApplyJobTerminal(ctx, jobID)
	if err != nil || current.Status != configuration.ApplyTerminalStatus("queued") {
		return current, err
	}
	c.mu.Lock()
	shouldComplete := !c.done
	if shouldComplete {
		c.done = true
	}
	c.mu.Unlock()
	if shouldComplete {
		finished := time.Now().UTC()
		if _, err := c.pool.Exec(ctx, `
			update apply_jobs set status = 'applied', finished_at = $2
			where id = $1 and status = 'queued'
		`, jobID, finished); err != nil {
			return configuration.ApplyJobTerminal{}, err
		}
		if _, err := c.pool.Exec(ctx, `
			update config_revisions r set applied_at = $2
			from apply_jobs j where j.id = $1 and r.id = j.revision_id
		`, jobID, finished); err != nil {
			return configuration.ApplyJobTerminal{}, err
		}
		if _, err := c.pool.Exec(ctx, `
			update installation_settings s set
				current_desired_revision_id = j.revision_id::text,
				current_applied_revision_id = j.revision_id::text,
				updated_at = $2
			from apply_jobs j where j.id = $1 and s.id = 'default'
		`, jobID, finished); err != nil {
			return configuration.ApplyJobTerminal{}, err
		}
	}
	return c.store.GetApplyJobTerminal(ctx, jobID)
}

func seedPNE9OrdinaryApply(ctx context.Context, pool *pgxpool.Pool, primaryID uuid.UUID) error {
	desired := domain.ConfigurationSnapshot{
		Settings: domain.Settings{
			ForwardingRules:     []domain.ForwardingRule{},
			RetentionMaxAgeDays: 7,
			RetentionMaxSizeMb:  50,
		},
		Zones:        []domain.ZoneState{},
		Streams:      []domain.StreamRoute{},
		Certificates: []domain.CertificateStatus{},
	}
	body := []byte(domain.StableStringify(desired))
	checksum := domain.ChecksumSnapshot(desired)
	revisionID := uuid.New()
	jobID := uuid.New()
	var revisionNumber int
	if err := pool.QueryRow(ctx, `select coalesce(max(revision_number), 0) + 1 from config_revisions`).Scan(&revisionNumber); err != nil {
		return err
	}
	finished := time.Now().UTC()
	if _, err := pool.Exec(ctx, `
		insert into config_revisions (
			id, revision_number, checksum, snapshot, actor_user_id, source,
			source_primary_id, source_node_id, source_revision_id, applied_at
		) values ($1, $2, $3, $4, null, 'ordinary', $5, null, null, $6)
	`, revisionID, revisionNumber, checksum, body, primaryID, finished); err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, `
		insert into apply_jobs (
			id, revision_id, actor_user_id, target, status, correlation_id,
			source, source_primary_id, source_node_id, source_revision_id,
			finished_at
		) values ($1, $2, null, 'combined', 'applied', $3, 'ordinary', $4, null, null, $5)
	`, jobID, revisionID, uuid.NewString(), primaryID, finished); err != nil {
		return err
	}
	_, err := pool.Exec(ctx, `
		insert into installation_settings (id, forwarding_rules, retention_max_age_days, retention_max_size_mb,
			current_desired_revision_id, current_applied_revision_id)
		values ('default', '[]'::jsonb, 7, 50, $1, $1)
		on conflict (id) do update set
			current_desired_revision_id = excluded.current_desired_revision_id,
			current_applied_revision_id = excluded.current_applied_revision_id,
			updated_at = now()
	`, revisionID)
	return err
}

func readPNE9Publication(ctx context.Context, pool *pgxpool.Pool) (pne9Publication, error) {
	var result pne9Publication
	var snapshotVersion, replicationVersion int
	var body []byte
	if err := pool.QueryRow(ctx, `
		select a.id::text, a.snapshot_body, a.content_hash, a.snapshot_version,
			a.replication_version, a.leadership_generation, a.revision_id::text,
			a.apply_job_id::text, a.applied_at
		from applied_snapshots a
		where a.status::text = 'applied' and a.snapshot_body is not null
		order by a.applied_at desc, a.id desc limit 1
	`).Scan(&result.SnapshotID, &body, &result.ContentHash, &snapshotVersion,
		&replicationVersion, &result.Generation, &result.RevisionID, &result.ApplyJobID,
		&result.AppliedAt); err != nil {
		return pne9Publication{}, err
	}
	if len(body) == 0 {
		return pne9Publication{}, errors.New("canonical publication body is empty")
	}
	envelope, err := replicationsnapshot.Unmarshal(body)
	if err != nil || envelope.Validate() != nil {
		return pne9Publication{}, errors.New("canonical publication is not a valid envelope")
	}
	result.Body = append([]byte(nil), body...)
	result.SnapshotVer = snapshotVersion
	result.ReplicationVer = replicationVersion
	return result, nil
}

func installPNE9Credential(ctx context.Context, pool *pgxpool.Pool, ownerID string, primary *pne9Installation, node *pne9Installation, publication pne9Publication) (string, error) {
	credential, err := syncpublication.GenerateNodeCredential()
	if err != nil {
		return "", err
	}
	bearer := credential.BearerCopy()
	credentialID := credential.ID()
	credentialHash := credential.Hash()
	credential.Destroy()
	if bearer == "" || credentialID == "" || credentialHash == "" {
		return "", errors.New("generated node credential is incomplete")
	}

	attemptID := uuid.New()
	tokenID := uuid.New()
	createdAt := time.Now().UTC().Add(-time.Minute)
	expiresAt := createdAt.Add(time.Hour)
	tokenHash := sha256.Sum256([]byte("pne9-lineage-token"))
	grantPayloadHash := sha256.Sum256([]byte("pne9-lineage-payload"))
	transaction, err := pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer transaction.Rollback(ctx)
	if _, err := transaction.Exec(ctx, `
		insert into enrollment_tokens (
			id, token_selector, token_hash, hash_version, created_by_user_id,
			created_at, expires_at, consumed_at, consumed_by_attempt_id
		) values ($1, $2, $3, $4, $5, $6, $7, $6, $8)
	`, tokenID, fmt.Sprintf("pne9%s", strings.ReplaceAll(tokenID.String(), "-", ""))[:16], fmt.Sprintf("%x", tokenHash), configuration.EnrollmentTokenHashVersion, ownerID, createdAt, expiresAt, attemptID); err != nil {
		return "", err
	}
	if _, err := transaction.Exec(ctx, `
		insert into enrollment_attempts (
			id, state, primary_url, expected_primary_id, verified_primary_id,
			verified_primary_node_id, verified_leadership_generation, local_node_ip,
			ephemeral_private_key_wrapped, cluster_key_id, created_at, updated_at
		) values ($1, 'committed', $2, $3, $3, $4, $5, '127.0.0.1', 'pne9-test', $6, $7, $7)
	`, attemptID, primary.tlsURL, primary.identityID, primary.nodeID, publication.Generation, primary.clusterKey, createdAt); err != nil {
		return "", err
	}
	if _, err := transaction.Exec(ctx, `
		insert into node_credentials (id, node_id, credential_hash, hash_version, created_at)
		values ($1, $2, $3, $4, $5)
	`, credentialID, node.nodeID, credentialHash, syncpublication.NodeCredentialHashVersion, createdAt); err != nil {
		return "", err
	}
	if _, err := transaction.Exec(ctx, `
		insert into enrolled_nodes (
			node_id, installation_id, primary_id, credential_id, enrolled_at, created_by_attempt_id
		) values ($1, $2, $3, $4, $5, $6)
	`, node.nodeID, node.identityID, primary.identityID, credentialID, createdAt, attemptID); err != nil {
		return "", err
	}
	if _, err := transaction.Exec(ctx, `
		insert into enrollment_grants (
			attempt_id, token_id, installation_id, node_id, primary_id, primary_generation,
			node_ephemeral_public_key, sealed_bootstrap_payload, payload_hash, created_at, expires_at
		) values ($1, $2, $3, $4, $5, $6, 'pne9-test', 'pne9-test', $7, $8, $9)
	`, attemptID, tokenID, node.identityID, node.nodeID, primary.identityID, publication.Generation, fmt.Sprintf("%x", grantPayloadHash), createdAt, expiresAt); err != nil {
		return "", err
	}
	if err := transaction.Commit(ctx); err != nil {
		return "", err
	}
	return bearer, nil
}

func revokePNE9Credential(ctx context.Context, pool *pgxpool.Pool, nodeID uuid.UUID) error {
	_, err := pool.Exec(ctx, `update node_credentials set revoked_at = now() where node_id = $1`, nodeID)
	return err
}

func assertPNE9Status(t *testing.T, label string, status int, body []byte, want int) {
	t.Helper()
	if status != want {
		t.Fatalf("%s status=%d want=%d body=%s", label, status, want, redactedDigest(body))
	}
}

func assertPNE9NoSecret(t *testing.T, label string, body []byte, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if secret != "" && strings.Contains(string(body), secret) {
			t.Fatalf("%s exposed secret len=%d hash=%x body=%s", label, len(secret), sha256.Sum256([]byte(secret)), redactedDigest(body))
		}
	}
}

func decodePNE9[T any](t *testing.T, label string, body []byte, value *T) {
	t.Helper()
	if err := json.Unmarshal(body, value); err != nil {
		t.Fatalf("decode %s: %v body=%s", label, err, redactedDigest(body))
	}
}

func isPNE9NoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

var _ configuration.TerminalApplyReader = (*pne9ApplyCompleter)(nil)
