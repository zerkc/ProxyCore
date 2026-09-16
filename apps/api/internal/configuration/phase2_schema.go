package configuration

// phase2SchemaDefinition is the additive SQL contract shared by EnsureSchema
// and the schema-parity tests. The Drizzle mirror and migration intentionally
// keep the same table, column, state, and index names.
type phase2SchemaDefinition struct {
	Statements []string
}

func phase2SchemaContract() phase2SchemaDefinition {
	return phase2SchemaDefinition{Statements: []string{
		`do $$ begin create type proxycore_enrollment_attempt_state as enum (
			'draft','verified','confirmed','exchanged','archived',
			'initial-apply-pending','committed','cancelled','recoverable','failed'
		); exception when duplicate_object then null; end $$;`,
		`do $$ begin create type proxycore_sync_trigger as enum ('enrollment','periodic','manual','restart'); exception when duplicate_object then null; end $$;`,
		`do $$ begin create type proxycore_sync_attempt_status as enum ('queued','running','current','applied','failed','pending-ack'); exception when duplicate_object then null; end $$;`,
		`do $$ begin create type proxycore_persistence_source as enum ('ordinary','import','sync'); exception when duplicate_object then null; end $$;`,
		`do $$ begin create type proxycore_applied_snapshot_status as enum ('pending','applied','rejected','rolled-back','archived'); exception when duplicate_object then null; end $$;`,

		`create table if not exists standalone_archives (
			id uuid primary key default gen_random_uuid(),
			snapshot_content_hash text,
			revision_id uuid references config_revisions(id),
			archive_blob text not null,
			capture_reason text not null,
			captured_at timestamptz not null default now(),
			retention_started_at timestamptz,
			expires_at timestamptz,
			restoration_eligible boolean not null default true,
			created_at timestamptz not null default now()
		);`,
		`create index if not exists standalone_archives_expiry_idx on standalone_archives (expires_at);`,

		`create table if not exists enrollment_attempts (
			id uuid primary key default gen_random_uuid(),
			state proxycore_enrollment_attempt_state not null default 'draft',
			primary_url text not null,
			expected_primary_id uuid,
			verified_primary_id uuid,
			verified_primary_node_id uuid,
			verified_leadership_generation bigint,
			verified_primary_tls_spki_sha256 text,
			verified_primary_ca_fingerprint text,
			preview_digest text,
			local_node_ip text not null,
			archive_id uuid references standalone_archives(id),
			ephemeral_private_key_wrapped text not null,
			bootstrap_payload text,
			node_credential_secret_id uuid references secrets(id),
			cluster_key_id uuid references cluster_keys(id),
			initial_snapshot_hash text,
			initial_snapshot_revision_id uuid references config_revisions(id),
			initial_apply_job_id uuid references apply_jobs(id),
			failure_code text,
			confirmed_at timestamptz,
			created_at timestamptz not null default now(),
			updated_at timestamptz not null default now(),
			constraint enrollment_attempts_state_check check (state in (
				'draft','verified','confirmed','exchanged','archived',
				'initial-apply-pending','committed','cancelled','recoverable','failed'
			))
		);`,
		`create unique index if not exists enrollment_attempts_one_active_idx
			on enrollment_attempts ((1))
			where state not in ('committed','cancelled','failed');`,

		`create table if not exists enrollment_tokens (
			id uuid primary key default gen_random_uuid(),
			token_selector text not null,
			token_hash text not null,
			hash_version text not null,
			created_by_user_id uuid not null references users(id),
			created_at timestamptz not null default now(),
			expires_at timestamptz not null,
			consumed_at timestamptz,
			consumed_by_attempt_id uuid references enrollment_attempts(id),
			revoked_at timestamptz,
			constraint enrollment_tokens_expiry_check check (expires_at > created_at)
		);`,
		`create unique index if not exists enrollment_tokens_selector_idx on enrollment_tokens (token_selector);`,

		`create table if not exists node_credentials (
			id uuid primary key default gen_random_uuid(),
			node_id uuid not null,
			credential_hash text not null,
			hash_version text not null,
			created_at timestamptz not null default now(),
			last_authenticated_at timestamptz,
			revoked_at timestamptz
		);`,
		`create unique index if not exists node_credentials_node_idx on node_credentials (node_id);`,

		`create table if not exists enrolled_nodes (
			node_id uuid primary key,
			installation_id uuid not null,
			primary_id uuid not null,
			display_name text,
			credential_id uuid not null references node_credentials(id),
			enrolled_at timestamptz not null default now(),
			revoked_at timestamptz,
			created_by_attempt_id uuid not null references enrollment_attempts(id),
			constraint enrolled_nodes_attempt_unique unique (created_by_attempt_id)
		);`,
		`create unique index if not exists enrolled_nodes_installation_idx on enrolled_nodes (installation_id);`,
		`create unique index if not exists enrolled_nodes_credential_idx on enrolled_nodes (credential_id);`,

		`create table if not exists enrollment_grants (
			attempt_id uuid primary key references enrollment_attempts(id),
			token_id uuid not null unique references enrollment_tokens(id),
			installation_id uuid not null,
			node_id uuid not null,
			primary_id uuid not null,
			primary_generation bigint not null,
			node_ephemeral_public_key text not null,
			sealed_bootstrap_payload text not null,
			payload_hash text not null,
			created_at timestamptz not null default now(),
			expires_at timestamptz not null,
			constraint enrollment_grants_expiry_check check (expires_at > created_at)
		);`,

		`create table if not exists node_snapshot_acks (
			node_id uuid not null references enrolled_nodes(node_id),
			content_hash text not null,
			snapshot_version integer not null,
			replication_version integer not null,
			revision_id uuid not null references config_revisions(id),
			leadership_generation bigint not null,
			applied_at timestamptz not null,
			received_at timestamptz not null default now(),
			primary key (node_id, content_hash)
		);`,

		`create table if not exists sync_attempts (
			id uuid primary key default gen_random_uuid(),
			node_id uuid not null,
			trigger proxycore_sync_trigger not null,
			status proxycore_sync_attempt_status not null default 'queued',
			source_primary_id uuid,
			leadership_generation bigint,
			snapshot_version integer,
			replication_version integer,
			content_hash text,
			revision_id uuid references config_revisions(id),
			apply_job_id uuid references apply_jobs(id),
			result_code text,
			started_at timestamptz,
			finished_at timestamptz,
			created_at timestamptz not null default now(),
			updated_at timestamptz not null default now(),
			constraint sync_attempts_trigger_check check (trigger in ('enrollment','periodic','manual','restart'))
		);`,
		`create index if not exists sync_attempts_node_created_idx on sync_attempts (node_id, created_at desc);`,

		`alter table node_state add column if not exists enrollment_token_hash text;`,
		`alter table node_state add column if not exists enrollment_attempt_id uuid references enrollment_attempts(id);`,
		`alter table node_state add column if not exists primary_url text;`,
		`alter table node_state add column if not exists primary_installation_id uuid;`,
		`alter table node_state add column if not exists primary_tls_spki_sha256 text;`,
		`alter table node_state add column if not exists credential_id uuid references node_credentials(id);`,
		`alter table node_state add column if not exists sync_enabled boolean not null default false;`,
		`alter table node_state add column if not exists last_attempt_at timestamptz;`,
		`alter table node_state add column if not exists last_success_at timestamptz;`,
		`alter table node_state add column if not exists consecutive_failures integer not null default 0;`,
		`alter table node_state add column if not exists next_attempt_at timestamptz;`,
		`alter table node_state add column if not exists last_error_code text;`,
		`create unique index if not exists node_state_enrollment_attempt_idx on node_state (enrollment_attempt_id);`,
		`create unique index if not exists node_state_credential_idx on node_state (credential_id);`,

		`alter table config_revisions add column if not exists source proxycore_persistence_source not null default 'ordinary';`,
		`alter table config_revisions add column if not exists source_primary_id uuid;`,
		`alter table config_revisions add column if not exists source_node_id uuid;`,
		`alter table config_revisions add column if not exists source_revision_id uuid references config_revisions(id);`,
		`alter table config_revisions add column if not exists snapshot_content_hash text;`,
		`alter table config_revisions add column if not exists snapshot_version integer;`,
		`alter table config_revisions add column if not exists replication_version integer;`,
		`alter table config_revisions add column if not exists leadership_generation bigint;`,
		`alter table apply_jobs add column if not exists source proxycore_persistence_source not null default 'ordinary';`,
		`alter table apply_jobs add column if not exists source_primary_id uuid;`,
		`alter table apply_jobs add column if not exists source_node_id uuid;`,
		`alter table apply_jobs add column if not exists source_revision_id uuid references config_revisions(id);`,
		`alter table apply_jobs add column if not exists snapshot_content_hash text;`,
		`alter table apply_jobs add column if not exists snapshot_version integer;`,
		`alter table apply_jobs add column if not exists replication_version integer;`,
		`alter table apply_jobs add column if not exists leadership_generation bigint;`,
		`do $$ begin alter table config_revisions add constraint config_revisions_source_check check (source in ('ordinary','import','sync')); exception when duplicate_object then null; end $$;`,
		`do $$ begin alter table apply_jobs add constraint apply_jobs_source_check check (source in ('ordinary','import','sync')); exception when duplicate_object then null; end $$;`,

		`alter table applied_snapshots add column if not exists status proxycore_applied_snapshot_status not null default 'applied';`,
		`alter table applied_snapshots add column if not exists apply_job_id uuid references apply_jobs(id);`,
		`alter table applied_snapshots add column if not exists failure_code text;`,
		`do $$ begin alter table applied_snapshots add constraint applied_snapshots_status_check check (status in ('pending','applied','rejected','rolled-back','archived')); exception when duplicate_object then null; end $$;`,
	}}
}
