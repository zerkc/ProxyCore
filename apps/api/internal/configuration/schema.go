package configuration

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// EnsureSchema creates the configuration tables in the current search_path.
// It mirrors the Drizzle migrations for test setup (the production schema comes
// from the Drizzle migrate one-shot). Requires the users table to already exist
// (see auth.PostgresStore.EnsureSchema) for the actor foreign keys.
func EnsureSchema(ctx context.Context, pool *pgxpool.Pool) error {
	statements := []string{
		`do $$ begin create type proxycore_record_type as enum ('A','AAAA','CNAME','TXT','MX','SRV'); exception when duplicate_object then null; end $$;`,
		`do $$ begin create type proxycore_stream_protocol as enum ('tcp','udp'); exception when duplicate_object then null; end $$;`,
		`do $$ begin create type proxycore_certificate_issuer as enum ('self-signed','uploaded','letsencrypt'); exception when duplicate_object then null; end $$;`,
		`do $$ begin create type proxycore_certificate_challenge as enum ('none','http-01','dns-01'); exception when duplicate_object then null; end $$;`,
		`do $$ begin create type proxycore_certificate_status as enum ('pending','issued','active','expired','failed'); exception when duplicate_object then null; end $$;`,
		`do $$ begin create type proxycore_job_status as enum ('queued','validating','applying','applied','failed','rolled-back'); exception when duplicate_object then null; end $$;`,
		`do $$ begin create type proxycore_job_target as enum ('coredns','nginx','combined','certificate'); exception when duplicate_object then null; end $$;`,
		`create table if not exists installation_settings (
			id text primary key,
			ingress_ipv4 text,
			ingress_ipv6 text,
			default_resolver_pool jsonb,
			forwarding_rules jsonb,
			retention_max_age_days integer not null default 7,
			retention_max_size_mb integer not null default 50,
			current_desired_revision_id text,
			current_applied_revision_id text,
			created_at timestamptz not null default now(),
			updated_at timestamptz not null default now()
		);`,
		`create table if not exists zones (
			id uuid primary key default gen_random_uuid(),
			name text not null,
			enabled boolean not null default true,
			created_at timestamptz not null default now(),
			updated_at timestamptz not null default now()
		);`,
		`create unique index if not exists zones_name_idx on zones (name);`,
		`create table if not exists dns_records (
			id uuid primary key default gen_random_uuid(),
			zone_id uuid not null references zones(id) on delete cascade,
			name text not null,
			type proxycore_record_type not null,
			value jsonb not null,
			ttl integer not null default 300,
			enabled boolean not null default true,
			proxied boolean not null default false,
			proxy_settings jsonb,
			comment text,
			created_at timestamptz not null default now(),
			updated_at timestamptz not null default now()
		);`,
		`create unique index if not exists dns_records_zone_name_id_idx on dns_records (zone_id, name, id);`,
		`create table if not exists stream_routes (
			id uuid primary key default gen_random_uuid(),
			enabled boolean not null default true,
			protocol proxycore_stream_protocol not null,
			listen_address text not null,
			listen_port integer not null,
			upstream jsonb not null,
			created_at timestamptz not null default now(),
			updated_at timestamptz not null default now()
		);`,
		`create unique index if not exists stream_routes_listener_idx on stream_routes (protocol, listen_address, listen_port);`,
		`create table if not exists secrets (
			id uuid primary key default gen_random_uuid(),
			purpose text not null,
			ciphertext text not null,
			created_at timestamptz not null default now(),
			updated_at timestamptz not null default now()
		);`,
		`create table if not exists certificates (
			id uuid primary key default gen_random_uuid(),
			hostnames jsonb not null,
			issuer proxycore_certificate_issuer not null,
			challenge proxycore_certificate_challenge not null,
			environment text not null default 'production',
			status proxycore_certificate_status not null default 'pending',
			expires_at timestamptz,
			renew_after timestamptz,
			key_secret_id uuid references secrets(id),
			certificate_pem text,
			failure_reason text,
			created_at timestamptz not null default now(),
			updated_at timestamptz not null default now()
		);`,
		`create table if not exists provider_connections (
			id uuid primary key default gen_random_uuid(),
			provider text not null,
			name text not null,
			secret_id uuid not null references secrets(id),
			scope text not null,
			enabled boolean not null default true,
			created_at timestamptz not null default now(),
			updated_at timestamptz not null default now()
		);`,
		`create table if not exists config_revisions (
			id uuid primary key default gen_random_uuid(),
			revision_number integer not null,
			checksum text not null,
			snapshot jsonb not null,
			actor_user_id uuid references users(id),
			created_at timestamptz not null default now(),
			applied_at timestamptz
		);`,
		`create unique index if not exists config_revisions_number_idx on config_revisions (revision_number);`,
		`create unique index if not exists config_revisions_checksum_idx on config_revisions (checksum);`,
		`create table if not exists apply_jobs (
			id uuid primary key default gen_random_uuid(),
			revision_id uuid not null references config_revisions(id),
			actor_user_id uuid references users(id),
			target proxycore_job_target not null,
			status proxycore_job_status not null default 'queued',
			correlation_id text not null,
			validation_output jsonb,
			apply_output jsonb,
			health_output jsonb,
			error_message text,
			claimed_at timestamptz,
			started_at timestamptz,
			finished_at timestamptz,
			created_at timestamptz not null default now()
		);`,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement); err != nil {
			return fmt.Errorf("ensure configuration schema: %w", err)
		}
	}
	return nil
}
