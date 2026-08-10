package configuration

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
)

const (
	installationID  = "default"
	revisionLockKey = 1_872_641
	jobChannel      = "proxycore_jobs"
)

// querier is satisfied by *pgxpool.Pool and pgx.Tx.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store is the Postgres-backed configuration store (ports PgConfigurationStore).
type Store struct {
	pool            *pgxpool.Pool
	secrets         secrets.Store
	masterKeyBase64 string
	defaultIngress  domain.Ingress
}

// New builds a configuration store.
func New(pool *pgxpool.Pool, masterKeyBase64 string, defaultIngress domain.Ingress) *Store {
	var secretStore secrets.Store
	if masterKeyBase64 != "" {
		secretStore = secrets.NewPgStore(pool, masterKeyBase64)
	}
	return &Store{
		pool:            pool,
		secrets:         secretStore,
		masterKeyBase64: masterKeyBase64,
		defaultIngress:  defaultIngress,
	}
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("generate uuid: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// GetSettings returns the installation settings, initializing ingress first.
func (s *Store) GetSettings(ctx context.Context) (domain.Settings, error) {
	if _, err := s.InitializeIngress(ctx, s.defaultIngress); err != nil {
		return domain.Settings{}, err
	}
	row, err := ensureSettings(ctx, s.pool)
	if err != nil {
		return domain.Settings{}, err
	}
	return toSettings(row)
}

// InitializeIngress sets the ingress once when it is still unset.
func (s *Store) InitializeIngress(ctx context.Context, ingress domain.Ingress) (bool, error) {
	if ingress.IPv4 == "" && ingress.IPv6 == "" {
		return false, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	current, err := ensureSettings(ctx, tx)
	if err != nil {
		return false, err
	}
	if current.ingressIPv4 != nil || current.ingressIPv6 != nil {
		return false, nil
	}
	if _, err := tx.Exec(ctx,
		`update installation_settings set ingress_ipv4 = $2, ingress_ipv6 = $3, updated_at = now() where id = $1`,
		installationID, nullableString(ingress.IPv4), nullableString(ingress.IPv6),
	); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// UpdateSettings merges and persists the settings patch.
func (s *Store) UpdateSettings(ctx context.Context, patch SettingsPatch) (domain.Settings, error) {
	current, err := s.GetSettings(ctx)
	if err != nil {
		return domain.Settings{}, err
	}
	next := current
	if patch.Ingress != nil {
		if patch.Ingress.IPv4 != "" {
			next.Ingress.IPv4 = patch.Ingress.IPv4
		}
		if patch.Ingress.IPv6 != "" {
			next.Ingress.IPv6 = patch.Ingress.IPv6
		}
	}
	if patch.ForwardingRules != nil {
		validated, err := domain.ValidateForwardingRules(patch.ForwardingRules)
		if err != nil {
			return domain.Settings{}, err
		}
		next.ForwardingRules = validated
	}
	if patch.DefaultPool != nil {
		validated, err := domain.ValidateResolverPool(*patch.DefaultPool)
		if err != nil {
			return domain.Settings{}, err
		}
		next.DefaultPool = &validated
	}
	if patch.RetentionMaxAgeDays != nil {
		next.RetentionMaxAgeDays = *patch.RetentionMaxAgeDays
	}
	if patch.RetentionMaxSizeMb != nil {
		next.RetentionMaxSizeMb = *patch.RetentionMaxSizeMb
	}
	if next.RetentionMaxAgeDays < 1 || next.RetentionMaxSizeMb < 1 {
		return domain.Settings{}, errors.New("Retention limits must be positive")
	}

	if _, err := ensureSettings(ctx, s.pool); err != nil {
		return domain.Settings{}, err
	}
	defaultPoolJSON, err := marshalOrNull(next.DefaultPool)
	if err != nil {
		return domain.Settings{}, err
	}
	forwardingJSON, err := json.Marshal(next.ForwardingRules)
	if err != nil {
		return domain.Settings{}, err
	}
	if _, err := s.pool.Exec(ctx, `
		update installation_settings set
			ingress_ipv4 = $2,
			ingress_ipv6 = $3,
			default_resolver_pool = $4,
			forwarding_rules = $5,
			retention_max_age_days = $6,
			retention_max_size_mb = $7,
			updated_at = now()
		where id = $1
	`, installationID,
		nullableString(next.Ingress.IPv4), nullableString(next.Ingress.IPv6),
		defaultPoolJSON, forwardingJSON, next.RetentionMaxAgeDays, next.RetentionMaxSizeMb,
	); err != nil {
		return domain.Settings{}, err
	}
	return next, nil
}

// SettingsPatch carries a partial settings update.
type SettingsPatch struct {
	Ingress             *domain.Ingress
	DefaultPool         *domain.ResolverPool
	ForwardingRules     []domain.ForwardingRule
	RetentionMaxAgeDays *int
	RetentionMaxSizeMb  *int
}

// ListZones returns zones with their records.
func (s *Store) ListZones(ctx context.Context) ([]domain.ZoneState, error) {
	return listZones(ctx, s.pool)
}

// CreateZone inserts a zone and enqueues an apply.
func (s *Store) CreateZone(ctx context.Context, name, actorUserID string) (AutoApplyResult[domain.ZoneState], error) {
	var result AutoApplyResult[domain.ZoneState]
	normalized, err := domain.NormalizeDNSName(name, false)
	if err != nil {
		return result, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)

	id := newUUID()
	var zoneName string
	var enabled bool
	if err := tx.QueryRow(ctx,
		`insert into zones (id, name, enabled) values ($1, $2, true) returning name, enabled`,
		id, normalized,
	).Scan(&zoneName, &enabled); err != nil {
		return result, err
	}
	apply, err := createApplyJobInTransaction(ctx, tx, actorUserID)
	if err != nil {
		return result, err
	}
	if err := tx.Commit(ctx); err != nil {
		return result, err
	}
	result.Value = domain.ZoneState{ID: id, Name: zoneName, Enabled: enabled, Records: []domain.DNSRecord{}}
	result.Apply = apply
	return result, nil
}

// GetZone returns one zone and its records.
func (s *Store) GetZone(ctx context.Context, id string) (domain.ZoneState, error) {
	return getZone(ctx, s.pool, id)
}

// ListStreams returns validated stream routes.
func (s *Store) ListStreams(ctx context.Context) ([]domain.StreamRoute, error) {
	rows, err := s.pool.Query(ctx, `
		select id::text, enabled, protocol::text, listen_address, listen_port, upstream
		from stream_routes order by listen_port asc
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	routes := []domain.StreamRoute{}
	for rows.Next() {
		route, err := scanStream(rows)
		if err != nil {
			return nil, err
		}
		routes = append(routes, route)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return domain.ValidateStreamRoutes(routes)
}

// AddStream upserts a stream route (no auto-apply, matching Node).
func (s *Store) AddStream(ctx context.Context, input domain.StreamRoute) (domain.StreamRoute, error) {
	current, err := s.ListStreams(ctx)
	if err != nil {
		return domain.StreamRoute{}, err
	}
	route := input
	if route.ID == "" {
		route.ID = newUUID()
	}
	next := make([]domain.StreamRoute, 0, len(current)+1)
	for _, item := range current {
		if item.ID != route.ID {
			next = append(next, item)
		}
	}
	next = append(next, route)
	validated, err := domain.ValidateStreamRoutes(next)
	if err != nil {
		return domain.StreamRoute{}, err
	}
	var saved domain.StreamRoute
	for _, item := range validated {
		if item.ID == route.ID {
			saved = item
		}
	}
	upstreamJSON, err := json.Marshal(saved.Upstream)
	if err != nil {
		return domain.StreamRoute{}, err
	}
	if _, err := s.pool.Exec(ctx, `
		insert into stream_routes (id, enabled, protocol, listen_address, listen_port, upstream)
		values ($1, $2, $3::proxycore_stream_protocol, $4, $5, $6)
		on conflict (id) do update set
			enabled = excluded.enabled,
			protocol = excluded.protocol,
			listen_address = excluded.listen_address,
			listen_port = excluded.listen_port,
			upstream = excluded.upstream,
			updated_at = now()
	`, saved.ID, saved.Enabled, saved.Protocol, saved.ListenAddress, saved.ListenPort, upstreamJSON); err != nil {
		return domain.StreamRoute{}, err
	}
	return saved, nil
}

// DeleteStream removes a stream route.
func (s *Store) DeleteStream(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `delete from stream_routes where id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("Stream not found: %s", id)
	}
	return nil
}

// CreateApplyJob enqueues a new apply job for the current desired state.
func (s *Store) CreateApplyJob(ctx context.Context, actorUserID string) (ApplyResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ApplyResult{}, err
	}
	defer tx.Rollback(ctx)
	apply, err := createApplyJobInTransaction(ctx, tx, actorUserID)
	if err != nil {
		return ApplyResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ApplyResult{}, err
	}
	return apply, nil
}

// Status aggregates the current desired/applied state.
func (s *Store) Status(ctx context.Context) (StatusResult, error) {
	settingsRow, err := ensureSettings(ctx, s.pool)
	if err != nil {
		return StatusResult{}, err
	}
	var result StatusResult
	if settingsRow.currentDesiredRevisionID != nil {
		rev, err := getRevision(ctx, s.pool, *settingsRow.currentDesiredRevisionID)
		if err != nil {
			return StatusResult{}, err
		}
		result.DesiredRevision = rev
	}
	if settingsRow.currentAppliedRevisionID != nil {
		rev, err := getRevision(ctx, s.pool, *settingsRow.currentAppliedRevisionID)
		if err != nil {
			return StatusResult{}, err
		}
		result.AppliedRevision = rev
	}
	if result.Jobs, err = listJobs(ctx, s.pool); err != nil {
		return StatusResult{}, err
	}
	if result.Settings, err = s.GetSettings(ctx); err != nil {
		return StatusResult{}, err
	}
	if result.Zones, err = s.ListZones(ctx); err != nil {
		return StatusResult{}, err
	}
	if result.Streams, err = s.ListStreams(ctx); err != nil {
		return StatusResult{}, err
	}
	if result.Certificates, err = s.ListCertificates(ctx); err != nil {
		return StatusResult{}, err
	}
	return result, nil
}

// Snapshot returns the current desired configuration snapshot.
func (s *Store) Snapshot(ctx context.Context) (domain.ConfigurationSnapshot, error) {
	return readDesiredSnapshot(ctx, s.pool)
}

func createApplyJobInTransaction(ctx context.Context, tx querier, actorUserID string) (ApplyResult, error) {
	if _, err := ensureSettings(ctx, tx); err != nil {
		return ApplyResult{}, err
	}
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, revisionLockKey); err != nil {
		return ApplyResult{}, err
	}
	var settingsID string
	if err := tx.QueryRow(ctx,
		`select id from installation_settings where id = $1 for update`, installationID,
	).Scan(&settingsID); err != nil {
		return ApplyResult{}, err
	}
	snapshot, err := readDesiredSnapshot(ctx, tx)
	if err != nil {
		return ApplyResult{}, err
	}
	if snapshot.Settings.DefaultPool == nil {
		return ApplyResult{}, errors.New("Configure a default resolver pool before apply")
	}
	checksum := domain.ChecksumSnapshot(snapshot)
	stableJSON := domain.StableStringify(snapshot)

	var revisionID string
	err = tx.QueryRow(ctx, `select id::text from config_revisions where checksum = $1 limit 1`, checksum).Scan(&revisionID)
	if errors.Is(err, pgx.ErrNoRows) {
		var maxNumber *int
		if err := tx.QueryRow(ctx, `select max(revision_number) from config_revisions`).Scan(&maxNumber); err != nil {
			return ApplyResult{}, err
		}
		nextNumber := 1
		if maxNumber != nil {
			nextNumber = *maxNumber + 1
		}
		revisionID = newUUID()
		if _, err := tx.Exec(ctx, `
			insert into config_revisions (id, revision_number, checksum, snapshot, actor_user_id)
			values ($1, $2, $3, $4, $5)
		`, revisionID, nextNumber, checksum, []byte(stableJSON), nullableString(actorUserID)); err != nil {
			return ApplyResult{}, err
		}
	} else if err != nil {
		return ApplyResult{}, err
	}

	if _, err := tx.Exec(ctx,
		`update installation_settings set current_desired_revision_id = $2, updated_at = now() where id = $1`,
		settingsID, revisionID,
	); err != nil {
		return ApplyResult{}, err
	}

	jobID := newUUID()
	row := tx.QueryRow(ctx, `
		insert into apply_jobs (id, revision_id, actor_user_id, target, status, correlation_id)
		values ($1, $2, $3, 'combined', 'queued', $4)
		returning id::text, revision_id::text, actor_user_id::text, target::text, status::text, correlation_id,
			created_at, claimed_at, started_at, finished_at, validation_output, apply_output, health_output, error_message
	`, jobID, revisionID, nullableString(actorUserID), newUUID())
	job, err := scanJob(row)
	if err != nil {
		return ApplyResult{}, err
	}
	if _, err := tx.Exec(ctx, `select pg_notify($1, $2)`, jobChannel, job.ID); err != nil {
		return ApplyResult{}, err
	}
	return ApplyResult{RevisionID: revisionID, Job: job}, nil
}

// ForceApply is exposed for the apply route; identical to CreateApplyJob.
func (s *Store) ForceApply(ctx context.Context, actorUserID string) (ApplyResult, error) {
	return s.CreateApplyJob(ctx, actorUserID)
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func marshalOrNull(value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	// A nil pointer inside an interface still needs an explicit null.
	if pool, ok := value.(*domain.ResolverPool); ok && pool == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}
