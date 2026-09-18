package configuration

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
)

func TestPhase2SchemaContractCoversAdditivePersistence(t *testing.T) {
	contract := phase2SchemaContract()

	for _, table := range []string{
		"internal_ca_enrollment_state",
		"enrollment_tokens",
		"enrolled_nodes",
		"node_credentials",
		"enrollment_grants",
		"node_snapshot_acks",
		"enrollment_attempts",
		"sync_attempts",
		"standalone_archives",
	} {
		if !containsSQLTable(contract.Statements, table) {
			t.Errorf("phase 2 schema is missing table %q", table)
		}
	}

	for _, column := range []string{
		"established_at",
		"enrollment_certificate_pem",
		"enrollment_key_secret_id",
		"enrollment_attempt_id",
		"primary_url",
		"primary_installation_id",
		"primary_tls_spki_sha256",
		"credential_id",
		"sync_enabled",
		"last_attempt_at",
		"last_success_at",
		"consecutive_failures",
		"next_attempt_at",
		"last_error_code",
		"verified_preview_digest",
		"source",
		"source_primary_id",
		"source_node_id",
		"source_revision_id",
		"snapshot_content_hash",
		"snapshot_version",
		"replication_version",
		"leadership_generation",
		"status",
	} {
		if !containsSQLColumn(contract.Statements, column) {
			t.Errorf("phase 2 schema is missing column %q", column)
		}
	}

	if !containsSQLText(contract.Statements, "enrollment_attempts_one_active_idx") {
		t.Fatal("phase 2 schema must enforce one non-terminal enrollment attempt")
	}
	if !containsSQLText(contract.Statements, "enrollment_attempts_state_check") {
		t.Fatal("phase 2 schema must check enrollment attempt states")
	}
	if !containsSQLText(contract.Statements, "alter table enrollment_grants add column if not exists verified_preview_digest") {
		t.Fatal("phase 2 schema must upgrade existing grants with the verified preview digest")
	}
	if !containsSQLText(contract.Statements, "sync_attempts_trigger_check") {
		t.Fatal("phase 2 schema must check sync triggers")
	}
	if !containsSQLText(contract.Statements, "source_check") {
		t.Fatal("phase 2 schema must check source attribution values")
	}
	if !containsSQLText(contract.Statements, "enrollment_token_hash") {
		t.Fatal("legacy enrollment_token_hash must remain readable")
	}
}

func TestPhase2SchemaKeepsPrimaryAttemptIDsCrossInstallation(t *testing.T) {
	contract := phase2SchemaContract()
	for _, table := range []string{"enrollment_tokens", "enrollment_grants", "enrolled_nodes"} {
		for _, statement := range contract.Statements {
			lower := strings.ToLower(statement)
			if strings.Contains(lower, "create table if not exists "+table) &&
				strings.Contains(lower, "references enrollment_attempts") {
				t.Fatalf("%s still references node-local enrollment_attempts: %q", table, statement)
			}
		}
	}
	for _, constraint := range []string{
		"enrollment_tokens_consumed_by_attempt_id_enrollment_attempts_id_fk",
		"enrollment_tokens_consumed_by_attempt_id_fkey",
		"enrollment_grants_attempt_id_enrollment_attempts_id_fk",
		"enrollment_grants_attempt_id_fkey",
		"enrolled_nodes_created_by_attempt_id_enrollment_attempts_id_fk",
		"enrolled_nodes_created_by_attempt_id_fkey",
	} {
		if !containsSQLText(contract.Statements, "drop constraint if exists "+constraint) {
			t.Fatalf("missing idempotent legacy constraint drop %q", constraint)
		}
	}
}

func TestPhase2SchemaContractIsIdempotentAndNonDestructive(t *testing.T) {
	contract := phase2SchemaContract()
	for _, statement := range contract.Statements {
		if !strings.Contains(strings.ToLower(statement), "create") &&
			!strings.Contains(strings.ToLower(statement), "alter") {
			t.Errorf("phase 2 schema statement is not additive: %q", statement)
		}
		lower := strings.ToLower(statement)
		if (strings.Contains(lower, "drop ") && !strings.Contains(lower, "drop constraint if exists")) ||
			strings.Contains(lower, "truncate ") {
			t.Errorf("phase 2 schema statement is destructive: %q", statement)
		}
	}
}

func TestEnsureSchemaIsIdempotentAgainstMigratedPostgres(t *testing.T) {
	databaseURL := os.Getenv("PHASE2_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("PHASE2_DATABASE_URL is not configured")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	schema := "phase2_schema_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ident := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "create schema "+ident); err != nil {
		admin.Close()
		t.Fatalf("create isolated schema: %v", err)
	}
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		admin.Close()
		t.Fatalf("parse database config: %v", err)
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		admin.Close()
		t.Fatalf("connect isolated schema: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "drop schema if exists "+ident+" cascade")
		admin.Close()
	})
	if err := auth.NewPostgresStore(pool).EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure auth schema: %v", err)
	}
	if err := EnsureSchema(ctx, pool); err != nil {
		t.Fatalf("first EnsureSchema: %v", err)
	}
	for _, statement := range []string{
		`alter table enrollment_tokens add constraint enrollment_tokens_consumed_by_attempt_id_fkey foreign key (consumed_by_attempt_id) references enrollment_attempts(id)`,
		`alter table enrollment_grants add constraint enrollment_grants_attempt_id_fkey foreign key (attempt_id) references enrollment_attempts(id)`,
		`alter table enrolled_nodes add constraint enrolled_nodes_created_by_attempt_id_fkey foreign key (created_by_attempt_id) references enrollment_attempts(id)`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("simulate legacy cross-install FK: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `alter table enrollment_grants drop column verified_preview_digest`); err != nil {
		t.Fatalf("simulate pre-0007 schema: %v", err)
	}
	if err := EnsureSchema(ctx, pool); err != nil {
		t.Fatalf("upgrade EnsureSchema: %v", err)
	}
	if err := EnsureSchema(ctx, pool); err != nil {
		t.Fatalf("second EnsureSchema: %v", err)
	}
	var previewDigestColumn string
	if err := pool.QueryRow(ctx, `
		select is_nullable from information_schema.columns
		where table_schema = current_schema() and table_name = 'enrollment_grants'
		  and column_name = 'verified_preview_digest'
	`).Scan(&previewDigestColumn); err != nil {
		t.Fatalf("read upgraded preview digest column: %v", err)
	}
	if previewDigestColumn != "YES" {
		t.Fatalf("upgraded preview digest nullability = %q, want YES for legacy rows", previewDigestColumn)
	}
	var legacyFKCount int
	if err := pool.QueryRow(ctx, `
		select count(*) from pg_constraint
		where conname in (
			'enrollment_tokens_consumed_by_attempt_id_fkey',
			'enrollment_grants_attempt_id_fkey',
			'enrolled_nodes_created_by_attempt_id_fkey'
		)
	`).Scan(&legacyFKCount); err != nil {
		t.Fatalf("read legacy cross-install FKs: %v", err)
	}
	if legacyFKCount != 0 {
		t.Fatalf("legacy cross-install FKs remaining = %d", legacyFKCount)
	}
	var enrollmentFKs int
	if err := pool.QueryRow(ctx, `select count(*) from pg_constraint where conrelid = 'internal_ca'::regclass and conname in ('internal_ca_enrollment_key_secret_id_fkey', 'internal_ca_enrollment_key_secret_id_secrets_id_fk')`).Scan(&enrollmentFKs); err != nil {
		t.Fatalf("query enrollment FK shape: %v", err)
	}
	if enrollmentFKs != 1 {
		t.Fatalf("expected one enrollment key FK, got %d", enrollmentFKs)
	}

	var legacyHash string
	if err := pool.QueryRow(ctx, `
		insert into node_state (id, enrollment_token_hash)
		values ('default', 'legacy-hash')
		on conflict (id) do update set enrollment_token_hash = excluded.enrollment_token_hash
		returning enrollment_token_hash
	`).Scan(&legacyHash); err != nil {
		t.Fatalf("preserve legacy node state: %v", err)
	}
	if legacyHash != "legacy-hash" {
		t.Fatalf("legacy enrollment token hash changed: %q", legacyHash)
	}

	var tableCount int
	if err := pool.QueryRow(ctx, `
		select count(*) from information_schema.tables
		where table_schema = current_schema() and table_name in (
			'internal_ca_enrollment_state', 'enrollment_tokens', 'enrolled_nodes', 'node_credentials', 'enrollment_grants',
			'node_snapshot_acks', 'enrollment_attempts', 'sync_attempts', 'standalone_archives'
		)
	`).Scan(&tableCount); err != nil {
		t.Fatalf("query phase 2 tables: %v", err)
	}
	if tableCount != 9 {
		t.Fatalf("expected all phase 2 tables, got %d", tableCount)
	}

	for _, indexName := range []string{
		"enrollment_attempts_one_active_idx",
		"enrollment_tokens_selector_idx",
		"enrolled_nodes_installation_idx",
		"enrolled_nodes_credential_idx",
		"node_credentials_node_idx",
		"standalone_archives_expiry_idx",
		"sync_attempts_node_created_idx",
	} {
		var present bool
		if err := pool.QueryRow(ctx, `
			select exists (
				select 1 from pg_indexes where schemaname = current_schema() and indexname = $1
			)
		`, indexName).Scan(&present); err != nil {
			t.Fatalf("query index %s: %v", indexName, err)
		}
		if !present {
			t.Fatalf("missing phase 2 index %s", indexName)
		}
	}

	var firstAttempt string
	if err := pool.QueryRow(ctx, `
		insert into enrollment_attempts (primary_url, local_node_ip, ephemeral_private_key_wrapped)
		values ('https://primary.example', '192.0.2.20', 'wrapped')
		returning id::text
	`).Scan(&firstAttempt); err != nil {
		t.Fatalf("insert active attempt: %v", err)
	}
	defer pool.Exec(ctx, `delete from enrollment_attempts where id = $1`, firstAttempt)
	if _, err := pool.Exec(ctx, `
		insert into enrollment_attempts (primary_url, local_node_ip, ephemeral_private_key_wrapped)
		values ('https://primary.example', '192.0.2.21', 'wrapped')
	`); err == nil {
		t.Fatal("expected one-active-attempt unique index to reject a concurrent attempt")
	}

	var committedAttemptOne, committedAttemptTwo string
	for _, item := range []struct {
		ip   string
		dest *string
	}{
		{ip: "192.0.2.30", dest: &committedAttemptOne},
		{ip: "192.0.2.31", dest: &committedAttemptTwo},
	} {
		if err := pool.QueryRow(ctx, `
			insert into enrollment_attempts (state, primary_url, local_node_ip, ephemeral_private_key_wrapped)
			values ('committed', 'https://primary.example', $1, 'wrapped')
			returning id::text
		`, item.ip).Scan(item.dest); err != nil {
			t.Fatalf("insert committed attempt: %v", err)
		}
	}
	var credentialOne, credentialTwo string
	if err := pool.QueryRow(ctx, `
		insert into node_credentials (node_id, credential_hash, hash_version)
		values (gen_random_uuid(), 'hash-one', 'sha256-v1') returning id::text
	`).Scan(&credentialOne); err != nil {
		t.Fatalf("insert first node credential: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		insert into node_credentials (node_id, credential_hash, hash_version)
		values (gen_random_uuid(), 'hash-two', 'sha256-v1') returning id::text
	`).Scan(&credentialTwo); err != nil {
		t.Fatalf("insert second node credential: %v", err)
	}
	installation := "00000000-0000-4000-8000-000000000010"
	primary := "00000000-0000-4000-8000-000000000011"
	var nodeOne string
	if err := pool.QueryRow(ctx, `
		insert into enrolled_nodes (node_id, installation_id, primary_id, credential_id, created_by_attempt_id)
		values (gen_random_uuid(), $1, $2, $3, $4) returning node_id::text
	`, installation, primary, credentialOne, committedAttemptOne).Scan(&nodeOne); err != nil {
		t.Fatalf("insert first enrolled node: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into enrolled_nodes (node_id, installation_id, primary_id, credential_id, created_by_attempt_id)
		values (gen_random_uuid(), $1, $2, $3, $4)
	`, installation, primary, credentialTwo, committedAttemptTwo); err == nil {
		t.Fatal("expected unique installation binding to reject a duplicate node")
	}
}

func containsSQLTable(statements []string, table string) bool {
	return containsSQLText(statements, "create table if not exists "+table)
}

func containsSQLColumn(statements []string, column string) bool {
	for _, statement := range statements {
		if strings.Contains(statement, column+" ") {
			return true
		}
	}
	return false
}

func containsSQLText(statements []string, text string) bool {
	for _, statement := range statements {
		if strings.Contains(statement, text) {
			return true
		}
	}
	return false
}
