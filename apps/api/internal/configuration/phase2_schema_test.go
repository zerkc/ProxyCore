package configuration

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPhase2SchemaContractCoversAdditivePersistence(t *testing.T) {
	contract := phase2SchemaContract()

	for _, table := range []string{
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

func TestPhase2SchemaContractIsIdempotentAndNonDestructive(t *testing.T) {
	contract := phase2SchemaContract()
	for _, statement := range contract.Statements {
		if !strings.Contains(strings.ToLower(statement), "create") &&
			!strings.Contains(strings.ToLower(statement), "alter") {
			t.Errorf("phase 2 schema statement is not additive: %q", statement)
		}
		if strings.Contains(strings.ToLower(statement), "drop ") ||
			strings.Contains(strings.ToLower(statement), "truncate ") {
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
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	if err := EnsureSchema(ctx, pool); err != nil {
		t.Fatalf("first EnsureSchema: %v", err)
	}
	if err := EnsureSchema(ctx, pool); err != nil {
		t.Fatalf("second EnsureSchema: %v", err)
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
		where table_schema = 'public' and table_name in (
			'enrollment_tokens', 'enrolled_nodes', 'node_credentials', 'enrollment_grants',
			'node_snapshot_acks', 'enrollment_attempts', 'sync_attempts', 'standalone_archives'
		)
	`).Scan(&tableCount); err != nil {
		t.Fatalf("query phase 2 tables: %v", err)
	}
	if tableCount != 8 {
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
				select 1 from pg_indexes where schemaname = 'public' and indexname = $1
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
