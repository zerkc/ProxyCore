CREATE TYPE "public"."proxycore_applied_snapshot_status" AS ENUM('pending', 'applied', 'rejected', 'rolled-back', 'archived');--> statement-breakpoint
CREATE TYPE "public"."proxycore_enrollment_attempt_state" AS ENUM('draft', 'verified', 'confirmed', 'exchanged', 'archived', 'initial-apply-pending', 'committed', 'cancelled', 'recoverable', 'failed');--> statement-breakpoint
CREATE TYPE "public"."proxycore_persistence_source" AS ENUM('ordinary', 'import', 'sync');--> statement-breakpoint
CREATE TYPE "public"."proxycore_sync_attempt_status" AS ENUM('queued', 'running', 'current', 'applied', 'failed', 'pending-ack');--> statement-breakpoint
CREATE TYPE "public"."proxycore_sync_trigger" AS ENUM('enrollment', 'periodic', 'manual', 'restart');--> statement-breakpoint
CREATE TYPE "public"."proxycore_topology_role" AS ENUM('standalone-primary', 'primary', 'primary-with-nodes', 'node', 'stale-primary');--> statement-breakpoint
CREATE TABLE "applied_snapshots" (
	"id" uuid PRIMARY KEY DEFAULT gen_random_uuid() NOT NULL,
	"source_primary_id" uuid NOT NULL,
	"leadership_generation" bigint NOT NULL,
	"snapshot_version" integer NOT NULL,
	"replication_version" integer NOT NULL,
	"content_hash" text NOT NULL,
	"revision_id" uuid,
	"status" "proxycore_applied_snapshot_status" DEFAULT 'applied' NOT NULL,
	"apply_job_id" uuid,
	"failure_code" text,
	"applied_at" timestamp with time zone DEFAULT now() NOT NULL,
	"discarded_at" timestamp with time zone,
	CONSTRAINT "applied_snapshots_status_check" CHECK ("applied_snapshots"."status" in ('pending', 'applied', 'rejected', 'rolled-back', 'archived'))
);
--> statement-breakpoint
CREATE TABLE "cluster_keys" (
	"id" uuid PRIMARY KEY DEFAULT gen_random_uuid() NOT NULL,
	"purpose" text NOT NULL,
	"wrapped_kek" text NOT NULL,
	"wrapping_key_version" integer NOT NULL,
	"created_at" timestamp with time zone DEFAULT now() NOT NULL,
	"retired_at" timestamp with time zone
);
--> statement-breakpoint
CREATE TABLE "enrolled_nodes" (
	"node_id" uuid PRIMARY KEY NOT NULL,
	"installation_id" uuid NOT NULL,
	"primary_id" uuid NOT NULL,
	"display_name" text,
	"credential_id" uuid NOT NULL,
	"enrolled_at" timestamp with time zone DEFAULT now() NOT NULL,
	"revoked_at" timestamp with time zone,
	"created_by_attempt_id" uuid NOT NULL
);
--> statement-breakpoint
CREATE TABLE "enrollment_attempts" (
	"id" uuid PRIMARY KEY DEFAULT gen_random_uuid() NOT NULL,
	"state" "proxycore_enrollment_attempt_state" DEFAULT 'draft' NOT NULL,
	"primary_url" text NOT NULL,
	"expected_primary_id" uuid,
	"verified_primary_id" uuid,
	"verified_primary_node_id" uuid,
	"verified_leadership_generation" bigint,
	"verified_primary_tls_spki_sha256" text,
	"verified_primary_ca_fingerprint" text,
	"preview_digest" text,
	"local_node_ip" text NOT NULL,
	"archive_id" uuid,
	"ephemeral_private_key_wrapped" text NOT NULL,
	"bootstrap_payload" text,
	"node_credential_secret_id" uuid,
	"cluster_key_id" uuid,
	"initial_snapshot_hash" text,
	"initial_snapshot_revision_id" uuid,
	"initial_apply_job_id" uuid,
	"failure_code" text,
	"confirmed_at" timestamp with time zone,
	"created_at" timestamp with time zone DEFAULT now() NOT NULL,
	"updated_at" timestamp with time zone DEFAULT now() NOT NULL,
	CONSTRAINT "enrollment_attempts_state_check" CHECK ("enrollment_attempts"."state" in ('draft', 'verified', 'confirmed', 'exchanged', 'archived', 'initial-apply-pending', 'committed', 'cancelled', 'recoverable', 'failed'))
);
--> statement-breakpoint
CREATE TABLE "enrollment_grants" (
	"attempt_id" uuid PRIMARY KEY NOT NULL,
	"token_id" uuid NOT NULL,
	"installation_id" uuid NOT NULL,
	"node_id" uuid NOT NULL,
	"primary_id" uuid NOT NULL,
	"primary_generation" bigint NOT NULL,
	"node_ephemeral_public_key" text NOT NULL,
	"sealed_bootstrap_payload" text NOT NULL,
	"payload_hash" text NOT NULL,
	"created_at" timestamp with time zone DEFAULT now() NOT NULL,
	"expires_at" timestamp with time zone NOT NULL,
	CONSTRAINT "enrollment_grants_token_id_unique" UNIQUE("token_id"),
	CONSTRAINT "enrollment_grants_expiry_check" CHECK ("enrollment_grants"."expires_at" > "enrollment_grants"."created_at")
);
--> statement-breakpoint
CREATE TABLE "enrollment_tokens" (
	"id" uuid PRIMARY KEY DEFAULT gen_random_uuid() NOT NULL,
	"token_selector" text NOT NULL,
	"token_hash" text NOT NULL,
	"hash_version" text NOT NULL,
	"created_by_user_id" uuid NOT NULL,
	"created_at" timestamp with time zone DEFAULT now() NOT NULL,
	"expires_at" timestamp with time zone NOT NULL,
	"consumed_at" timestamp with time zone,
	"consumed_by_attempt_id" uuid,
	"revoked_at" timestamp with time zone,
	CONSTRAINT "enrollment_tokens_expiry_check" CHECK ("enrollment_tokens"."expires_at" > "enrollment_tokens"."created_at")
);
--> statement-breakpoint
CREATE TABLE "installation_identity" (
	"id" text PRIMARY KEY NOT NULL,
	"installation_id" uuid NOT NULL,
	"node_id" uuid NOT NULL,
	"role" "proxycore_topology_role" DEFAULT 'standalone-primary' NOT NULL,
	"leadership_generation" bigint DEFAULT 1 NOT NULL,
	"latest_known_generation" bigint DEFAULT 1 NOT NULL,
	"cluster_key_id" uuid,
	"updated_at" timestamp with time zone DEFAULT now() NOT NULL
);
--> statement-breakpoint
CREATE TABLE "node_credentials" (
	"id" uuid PRIMARY KEY DEFAULT gen_random_uuid() NOT NULL,
	"node_id" uuid NOT NULL,
	"credential_hash" text NOT NULL,
	"hash_version" text NOT NULL,
	"created_at" timestamp with time zone DEFAULT now() NOT NULL,
	"last_authenticated_at" timestamp with time zone,
	"revoked_at" timestamp with time zone
);
--> statement-breakpoint
CREATE TABLE "node_snapshot_acks" (
	"node_id" uuid NOT NULL,
	"content_hash" text NOT NULL,
	"snapshot_version" integer NOT NULL,
	"replication_version" integer NOT NULL,
	"revision_id" uuid NOT NULL,
	"leadership_generation" bigint NOT NULL,
	"applied_at" timestamp with time zone NOT NULL,
	"received_at" timestamp with time zone DEFAULT now() NOT NULL,
	CONSTRAINT "node_snapshot_acks_node_id_content_hash_pk" PRIMARY KEY("node_id","content_hash")
);
--> statement-breakpoint
CREATE TABLE "node_state" (
	"id" text PRIMARY KEY NOT NULL,
	"enrolled_at" timestamp with time zone,
	"enrollment_token_hash" text,
	"enrollment_primary_id" uuid,
	"last_seen_at" timestamp with time zone,
	"last_applied_snapshot_id" uuid,
	"enrollment_attempt_id" uuid,
	"primary_url" text,
	"primary_installation_id" uuid,
	"primary_tls_spki_sha256" text,
	"credential_id" uuid,
	"sync_enabled" boolean DEFAULT false NOT NULL,
	"last_attempt_at" timestamp with time zone,
	"last_success_at" timestamp with time zone,
	"consecutive_failures" integer DEFAULT 0 NOT NULL,
	"next_attempt_at" timestamp with time zone,
	"last_error_code" text,
	"updated_at" timestamp with time zone DEFAULT now() NOT NULL
);
--> statement-breakpoint
CREATE TABLE "standalone_archives" (
	"id" uuid PRIMARY KEY DEFAULT gen_random_uuid() NOT NULL,
	"snapshot_content_hash" text,
	"revision_id" uuid,
	"archive_blob" text NOT NULL,
	"capture_reason" text NOT NULL,
	"captured_at" timestamp with time zone DEFAULT now() NOT NULL,
	"retention_started_at" timestamp with time zone,
	"expires_at" timestamp with time zone,
	"restoration_eligible" boolean DEFAULT true NOT NULL,
	"created_at" timestamp with time zone DEFAULT now() NOT NULL
);
--> statement-breakpoint
CREATE TABLE "sync_attempts" (
	"id" uuid PRIMARY KEY DEFAULT gen_random_uuid() NOT NULL,
	"node_id" uuid NOT NULL,
	"trigger" "proxycore_sync_trigger" NOT NULL,
	"status" "proxycore_sync_attempt_status" DEFAULT 'queued' NOT NULL,
	"source_primary_id" uuid,
	"leadership_generation" bigint,
	"snapshot_version" integer,
	"replication_version" integer,
	"content_hash" text,
	"revision_id" uuid,
	"apply_job_id" uuid,
	"result_code" text,
	"started_at" timestamp with time zone,
	"finished_at" timestamp with time zone,
	"created_at" timestamp with time zone DEFAULT now() NOT NULL,
	"updated_at" timestamp with time zone DEFAULT now() NOT NULL,
	CONSTRAINT "sync_attempts_trigger_check" CHECK ("sync_attempts"."trigger" in ('enrollment', 'periodic', 'manual', 'restart'))
);
--> statement-breakpoint
ALTER TABLE "apply_jobs" ADD COLUMN "source" "proxycore_persistence_source" DEFAULT 'ordinary' NOT NULL;--> statement-breakpoint
ALTER TABLE "apply_jobs" ADD COLUMN "source_primary_id" uuid;--> statement-breakpoint
ALTER TABLE "apply_jobs" ADD COLUMN "source_node_id" uuid;--> statement-breakpoint
ALTER TABLE "apply_jobs" ADD COLUMN "source_revision_id" uuid;--> statement-breakpoint
ALTER TABLE "apply_jobs" ADD COLUMN "snapshot_content_hash" text;--> statement-breakpoint
ALTER TABLE "apply_jobs" ADD COLUMN "snapshot_version" integer;--> statement-breakpoint
ALTER TABLE "apply_jobs" ADD COLUMN "replication_version" integer;--> statement-breakpoint
ALTER TABLE "apply_jobs" ADD COLUMN "leadership_generation" bigint;--> statement-breakpoint
ALTER TABLE "config_revisions" ADD COLUMN "source" "proxycore_persistence_source" DEFAULT 'ordinary' NOT NULL;--> statement-breakpoint
ALTER TABLE "config_revisions" ADD COLUMN "source_primary_id" uuid;--> statement-breakpoint
ALTER TABLE "config_revisions" ADD COLUMN "source_node_id" uuid;--> statement-breakpoint
ALTER TABLE "config_revisions" ADD COLUMN "source_revision_id" uuid;--> statement-breakpoint
ALTER TABLE "config_revisions" ADD COLUMN "snapshot_content_hash" text;--> statement-breakpoint
ALTER TABLE "config_revisions" ADD COLUMN "snapshot_version" integer;--> statement-breakpoint
ALTER TABLE "config_revisions" ADD COLUMN "replication_version" integer;--> statement-breakpoint
ALTER TABLE "config_revisions" ADD COLUMN "leadership_generation" bigint;--> statement-breakpoint
ALTER TABLE "applied_snapshots" ADD CONSTRAINT "applied_snapshots_revision_id_config_revisions_id_fk" FOREIGN KEY ("revision_id") REFERENCES "public"."config_revisions"("id") ON DELETE no action ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "applied_snapshots" ADD CONSTRAINT "applied_snapshots_apply_job_id_apply_jobs_id_fk" FOREIGN KEY ("apply_job_id") REFERENCES "public"."apply_jobs"("id") ON DELETE no action ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "enrolled_nodes" ADD CONSTRAINT "enrolled_nodes_credential_id_node_credentials_id_fk" FOREIGN KEY ("credential_id") REFERENCES "public"."node_credentials"("id") ON DELETE no action ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "enrolled_nodes" ADD CONSTRAINT "enrolled_nodes_created_by_attempt_id_enrollment_attempts_id_fk" FOREIGN KEY ("created_by_attempt_id") REFERENCES "public"."enrollment_attempts"("id") ON DELETE no action ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "enrollment_attempts" ADD CONSTRAINT "enrollment_attempts_archive_id_standalone_archives_id_fk" FOREIGN KEY ("archive_id") REFERENCES "public"."standalone_archives"("id") ON DELETE no action ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "enrollment_attempts" ADD CONSTRAINT "enrollment_attempts_node_credential_secret_id_secrets_id_fk" FOREIGN KEY ("node_credential_secret_id") REFERENCES "public"."secrets"("id") ON DELETE no action ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "enrollment_attempts" ADD CONSTRAINT "enrollment_attempts_cluster_key_id_cluster_keys_id_fk" FOREIGN KEY ("cluster_key_id") REFERENCES "public"."cluster_keys"("id") ON DELETE no action ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "enrollment_attempts" ADD CONSTRAINT "enrollment_attempts_initial_snapshot_revision_id_config_revisions_id_fk" FOREIGN KEY ("initial_snapshot_revision_id") REFERENCES "public"."config_revisions"("id") ON DELETE no action ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "enrollment_attempts" ADD CONSTRAINT "enrollment_attempts_initial_apply_job_id_apply_jobs_id_fk" FOREIGN KEY ("initial_apply_job_id") REFERENCES "public"."apply_jobs"("id") ON DELETE no action ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "enrollment_grants" ADD CONSTRAINT "enrollment_grants_attempt_id_enrollment_attempts_id_fk" FOREIGN KEY ("attempt_id") REFERENCES "public"."enrollment_attempts"("id") ON DELETE no action ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "enrollment_grants" ADD CONSTRAINT "enrollment_grants_token_id_enrollment_tokens_id_fk" FOREIGN KEY ("token_id") REFERENCES "public"."enrollment_tokens"("id") ON DELETE no action ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "enrollment_tokens" ADD CONSTRAINT "enrollment_tokens_created_by_user_id_users_id_fk" FOREIGN KEY ("created_by_user_id") REFERENCES "public"."users"("id") ON DELETE no action ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "enrollment_tokens" ADD CONSTRAINT "enrollment_tokens_consumed_by_attempt_id_enrollment_attempts_id_fk" FOREIGN KEY ("consumed_by_attempt_id") REFERENCES "public"."enrollment_attempts"("id") ON DELETE no action ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "node_snapshot_acks" ADD CONSTRAINT "node_snapshot_acks_node_id_enrolled_nodes_node_id_fk" FOREIGN KEY ("node_id") REFERENCES "public"."enrolled_nodes"("node_id") ON DELETE no action ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "node_snapshot_acks" ADD CONSTRAINT "node_snapshot_acks_revision_id_config_revisions_id_fk" FOREIGN KEY ("revision_id") REFERENCES "public"."config_revisions"("id") ON DELETE no action ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "standalone_archives" ADD CONSTRAINT "standalone_archives_revision_id_config_revisions_id_fk" FOREIGN KEY ("revision_id") REFERENCES "public"."config_revisions"("id") ON DELETE no action ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "sync_attempts" ADD CONSTRAINT "sync_attempts_revision_id_config_revisions_id_fk" FOREIGN KEY ("revision_id") REFERENCES "public"."config_revisions"("id") ON DELETE no action ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "sync_attempts" ADD CONSTRAINT "sync_attempts_apply_job_id_apply_jobs_id_fk" FOREIGN KEY ("apply_job_id") REFERENCES "public"."apply_jobs"("id") ON DELETE no action ON UPDATE no action;--> statement-breakpoint
CREATE UNIQUE INDEX "applied_snapshots_content_hash_idx" ON "applied_snapshots" USING btree ("content_hash");--> statement-breakpoint
CREATE UNIQUE INDEX "enrolled_nodes_installation_idx" ON "enrolled_nodes" USING btree ("installation_id");--> statement-breakpoint
CREATE UNIQUE INDEX "enrolled_nodes_credential_idx" ON "enrolled_nodes" USING btree ("credential_id");--> statement-breakpoint
CREATE UNIQUE INDEX "enrolled_nodes_attempt_unique" ON "enrolled_nodes" USING btree ("created_by_attempt_id");--> statement-breakpoint
CREATE UNIQUE INDEX "enrollment_attempts_one_active_idx" ON "enrollment_attempts" USING btree ((1)) WHERE "enrollment_attempts"."state" not in ('committed', 'cancelled', 'failed');--> statement-breakpoint
CREATE UNIQUE INDEX "enrollment_tokens_selector_idx" ON "enrollment_tokens" USING btree ("token_selector");--> statement-breakpoint
CREATE UNIQUE INDEX "node_credentials_node_idx" ON "node_credentials" USING btree ("node_id");--> statement-breakpoint
CREATE INDEX "standalone_archives_expiry_idx" ON "standalone_archives" USING btree ("expires_at");--> statement-breakpoint
CREATE INDEX "sync_attempts_node_created_idx" ON "sync_attempts" USING btree ("node_id","created_at");--> statement-breakpoint
ALTER TABLE "apply_jobs" ADD CONSTRAINT "apply_jobs_source_check" CHECK ("apply_jobs"."source" in ('ordinary', 'import', 'sync'));--> statement-breakpoint
ALTER TABLE "config_revisions" ADD CONSTRAINT "config_revisions_source_check" CHECK ("config_revisions"."source" in ('ordinary', 'import', 'sync'));