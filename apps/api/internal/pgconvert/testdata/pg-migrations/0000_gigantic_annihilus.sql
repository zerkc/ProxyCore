CREATE TYPE "public"."proxycore_certificate_status" AS ENUM('pending', 'issued', 'active', 'expired', 'failed');--> statement-breakpoint
CREATE TYPE "public"."proxycore_certificate_challenge" AS ENUM('none', 'http-01', 'dns-01');--> statement-breakpoint
CREATE TYPE "public"."proxycore_certificate_issuer" AS ENUM('self-signed', 'letsencrypt');--> statement-breakpoint
CREATE TYPE "public"."proxycore_job_status" AS ENUM('queued', 'validating', 'applying', 'applied', 'failed', 'rolled-back');--> statement-breakpoint
CREATE TYPE "public"."proxycore_job_target" AS ENUM('coredns', 'nginx', 'combined', 'certificate');--> statement-breakpoint
CREATE TYPE "public"."proxycore_record_type" AS ENUM('A', 'AAAA', 'CNAME', 'TXT', 'MX', 'SRV');--> statement-breakpoint
CREATE TYPE "public"."proxycore_role" AS ENUM('owner', 'operator');--> statement-breakpoint
CREATE TYPE "public"."proxycore_stream_protocol" AS ENUM('tcp', 'udp');--> statement-breakpoint
CREATE TABLE "apply_jobs" (
	"id" uuid PRIMARY KEY DEFAULT gen_random_uuid() NOT NULL,
	"revision_id" uuid NOT NULL,
	"actor_user_id" uuid,
	"target" "proxycore_job_target" NOT NULL,
	"status" "proxycore_job_status" DEFAULT 'queued' NOT NULL,
	"correlation_id" text NOT NULL,
	"validation_output" jsonb,
	"apply_output" jsonb,
	"health_output" jsonb,
	"error_message" text,
	"claimed_at" timestamp with time zone,
	"started_at" timestamp with time zone,
	"finished_at" timestamp with time zone,
	"created_at" timestamp with time zone DEFAULT now() NOT NULL
);
--> statement-breakpoint
CREATE TABLE "audit_events" (
	"id" uuid PRIMARY KEY DEFAULT gen_random_uuid() NOT NULL,
	"actor_user_id" uuid,
	"action" text NOT NULL,
	"resource_type" text NOT NULL,
	"resource_id" text,
	"before_value" jsonb,
	"after_value" jsonb,
	"correlation_id" text NOT NULL,
	"result" text NOT NULL,
	"created_at" timestamp with time zone DEFAULT now() NOT NULL
);
--> statement-breakpoint
CREATE TABLE "certificates" (
	"id" uuid PRIMARY KEY DEFAULT gen_random_uuid() NOT NULL,
	"hostnames" jsonb NOT NULL,
	"issuer" "proxycore_certificate_issuer" NOT NULL,
	"challenge" "proxycore_certificate_challenge" NOT NULL,
	"environment" text DEFAULT 'production' NOT NULL,
	"status" "proxycore_certificate_status" DEFAULT 'pending' NOT NULL,
	"expires_at" timestamp with time zone,
	"renew_after" timestamp with time zone,
	"key_secret_id" uuid,
	"certificate_pem" text,
	"created_at" timestamp with time zone DEFAULT now() NOT NULL,
	"updated_at" timestamp with time zone DEFAULT now() NOT NULL
);
--> statement-breakpoint
CREATE TABLE "config_revisions" (
	"id" uuid PRIMARY KEY DEFAULT gen_random_uuid() NOT NULL,
	"revision_number" integer NOT NULL,
	"checksum" text NOT NULL,
	"snapshot" jsonb NOT NULL,
	"actor_user_id" uuid,
	"created_at" timestamp with time zone DEFAULT now() NOT NULL,
	"applied_at" timestamp with time zone
);
--> statement-breakpoint
CREATE TABLE "dns_records" (
	"id" uuid PRIMARY KEY DEFAULT gen_random_uuid() NOT NULL,
	"zone_id" uuid NOT NULL,
	"name" text NOT NULL,
	"type" "proxycore_record_type" NOT NULL,
	"value" jsonb NOT NULL,
	"ttl" integer DEFAULT 300 NOT NULL,
	"enabled" boolean DEFAULT true NOT NULL,
	"proxied" boolean DEFAULT false NOT NULL,
	"proxy_settings" jsonb,
	"comment" text,
	"created_at" timestamp with time zone DEFAULT now() NOT NULL,
	"updated_at" timestamp with time zone DEFAULT now() NOT NULL
);
--> statement-breakpoint
CREATE TABLE "forwarding_rules" (
	"id" uuid PRIMARY KEY DEFAULT gen_random_uuid() NOT NULL,
	"suffix" text NOT NULL,
	"pool_id" uuid NOT NULL,
	"created_at" timestamp with time zone DEFAULT now() NOT NULL,
	"updated_at" timestamp with time zone DEFAULT now() NOT NULL
);
--> statement-breakpoint
CREATE TABLE "health_observations" (
	"id" uuid PRIMARY KEY DEFAULT gen_random_uuid() NOT NULL,
	"component" text NOT NULL,
	"status" text NOT NULL,
	"details" jsonb,
	"created_at" timestamp with time zone DEFAULT now() NOT NULL
);
--> statement-breakpoint
CREATE TABLE "installation_settings" (
	"id" text PRIMARY KEY NOT NULL,
	"ingress_ipv4" text,
	"ingress_ipv6" text,
	"default_resolver_pool" jsonb,
	"forwarding_rules" jsonb,
	"retention_max_age_days" integer DEFAULT 7 NOT NULL,
	"retention_max_size_mb" integer DEFAULT 50 NOT NULL,
	"current_desired_revision_id" text,
	"current_applied_revision_id" text,
	"created_at" timestamp with time zone DEFAULT now() NOT NULL,
	"updated_at" timestamp with time zone DEFAULT now() NOT NULL
);
--> statement-breakpoint
CREATE TABLE "operational_artifacts" (
	"id" uuid PRIMARY KEY DEFAULT gen_random_uuid() NOT NULL,
	"kind" text NOT NULL,
	"path" text NOT NULL,
	"size_bytes" integer NOT NULL,
	"revision_id" uuid,
	"created_at" timestamp with time zone DEFAULT now() NOT NULL
);
--> statement-breakpoint
CREATE TABLE "provider_connections" (
	"id" uuid PRIMARY KEY DEFAULT gen_random_uuid() NOT NULL,
	"provider" text NOT NULL,
	"name" text NOT NULL,
	"secret_id" uuid NOT NULL,
	"scope" text NOT NULL,
	"enabled" boolean DEFAULT true NOT NULL,
	"created_at" timestamp with time zone DEFAULT now() NOT NULL,
	"updated_at" timestamp with time zone DEFAULT now() NOT NULL
);
--> statement-breakpoint
CREATE TABLE "resolver_pools" (
	"id" uuid PRIMARY KEY DEFAULT gen_random_uuid() NOT NULL,
	"name" text NOT NULL,
	"endpoints" jsonb NOT NULL,
	"is_default" boolean DEFAULT false NOT NULL,
	"created_at" timestamp with time zone DEFAULT now() NOT NULL,
	"updated_at" timestamp with time zone DEFAULT now() NOT NULL
);
--> statement-breakpoint
CREATE TABLE "secrets" (
	"id" uuid PRIMARY KEY DEFAULT gen_random_uuid() NOT NULL,
	"purpose" text NOT NULL,
	"ciphertext" text NOT NULL,
	"created_at" timestamp with time zone DEFAULT now() NOT NULL,
	"updated_at" timestamp with time zone DEFAULT now() NOT NULL
);
--> statement-breakpoint
CREATE TABLE "sessions" (
	"id" uuid PRIMARY KEY DEFAULT gen_random_uuid() NOT NULL,
	"user_id" uuid NOT NULL,
	"token_hash" text NOT NULL,
	"expires_at" timestamp with time zone NOT NULL,
	"revoked_at" timestamp with time zone,
	"last_seen_at" timestamp with time zone,
	"created_at" timestamp with time zone DEFAULT now() NOT NULL
);
--> statement-breakpoint
CREATE TABLE "stream_routes" (
	"id" uuid PRIMARY KEY DEFAULT gen_random_uuid() NOT NULL,
	"enabled" boolean DEFAULT true NOT NULL,
	"protocol" "proxycore_stream_protocol" NOT NULL,
	"listen_address" text NOT NULL,
	"listen_port" integer NOT NULL,
	"upstream" jsonb NOT NULL,
	"created_at" timestamp with time zone DEFAULT now() NOT NULL,
	"updated_at" timestamp with time zone DEFAULT now() NOT NULL
);
--> statement-breakpoint
CREATE TABLE "users" (
	"id" uuid PRIMARY KEY DEFAULT gen_random_uuid() NOT NULL,
	"username" text NOT NULL,
	"password_hash" text NOT NULL,
	"role" "proxycore_role" NOT NULL,
	"active" boolean DEFAULT true NOT NULL,
	"created_at" timestamp with time zone DEFAULT now() NOT NULL,
	"updated_at" timestamp with time zone DEFAULT now() NOT NULL
);
--> statement-breakpoint
CREATE TABLE "zones" (
	"id" uuid PRIMARY KEY DEFAULT gen_random_uuid() NOT NULL,
	"name" text NOT NULL,
	"enabled" boolean DEFAULT true NOT NULL,
	"created_at" timestamp with time zone DEFAULT now() NOT NULL,
	"updated_at" timestamp with time zone DEFAULT now() NOT NULL
);
--> statement-breakpoint
ALTER TABLE "apply_jobs" ADD CONSTRAINT "apply_jobs_revision_id_config_revisions_id_fk" FOREIGN KEY ("revision_id") REFERENCES "public"."config_revisions"("id") ON DELETE no action ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "apply_jobs" ADD CONSTRAINT "apply_jobs_actor_user_id_users_id_fk" FOREIGN KEY ("actor_user_id") REFERENCES "public"."users"("id") ON DELETE no action ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "audit_events" ADD CONSTRAINT "audit_events_actor_user_id_users_id_fk" FOREIGN KEY ("actor_user_id") REFERENCES "public"."users"("id") ON DELETE no action ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "certificates" ADD CONSTRAINT "certificates_key_secret_id_secrets_id_fk" FOREIGN KEY ("key_secret_id") REFERENCES "public"."secrets"("id") ON DELETE no action ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "config_revisions" ADD CONSTRAINT "config_revisions_actor_user_id_users_id_fk" FOREIGN KEY ("actor_user_id") REFERENCES "public"."users"("id") ON DELETE no action ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "dns_records" ADD CONSTRAINT "dns_records_zone_id_zones_id_fk" FOREIGN KEY ("zone_id") REFERENCES "public"."zones"("id") ON DELETE cascade ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "forwarding_rules" ADD CONSTRAINT "forwarding_rules_pool_id_resolver_pools_id_fk" FOREIGN KEY ("pool_id") REFERENCES "public"."resolver_pools"("id") ON DELETE cascade ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "operational_artifacts" ADD CONSTRAINT "operational_artifacts_revision_id_config_revisions_id_fk" FOREIGN KEY ("revision_id") REFERENCES "public"."config_revisions"("id") ON DELETE no action ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "provider_connections" ADD CONSTRAINT "provider_connections_secret_id_secrets_id_fk" FOREIGN KEY ("secret_id") REFERENCES "public"."secrets"("id") ON DELETE no action ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "sessions" ADD CONSTRAINT "sessions_user_id_users_id_fk" FOREIGN KEY ("user_id") REFERENCES "public"."users"("id") ON DELETE cascade ON UPDATE no action;--> statement-breakpoint
CREATE UNIQUE INDEX "config_revisions_number_idx" ON "config_revisions" USING btree ("revision_number");--> statement-breakpoint
CREATE UNIQUE INDEX "config_revisions_checksum_idx" ON "config_revisions" USING btree ("checksum");--> statement-breakpoint
CREATE UNIQUE INDEX "dns_records_zone_name_id_idx" ON "dns_records" USING btree ("zone_id","name","id");--> statement-breakpoint
CREATE UNIQUE INDEX "forwarding_rules_suffix_idx" ON "forwarding_rules" USING btree ("suffix");--> statement-breakpoint
CREATE UNIQUE INDEX "resolver_pools_name_idx" ON "resolver_pools" USING btree ("name");--> statement-breakpoint
CREATE UNIQUE INDEX "sessions_token_hash_idx" ON "sessions" USING btree ("token_hash");--> statement-breakpoint
CREATE UNIQUE INDEX "stream_routes_listener_idx" ON "stream_routes" USING btree ("protocol","listen_address","listen_port");--> statement-breakpoint
CREATE UNIQUE INDEX "users_username_idx" ON "users" USING btree ("username");--> statement-breakpoint
CREATE UNIQUE INDEX "zones_name_idx" ON "zones" USING btree ("name");