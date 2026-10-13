ALTER TABLE "enrollment_grants" ADD COLUMN IF NOT EXISTS "verified_preview_digest" text;
--> statement-breakpoint
ALTER TABLE "enrollment_tokens" DROP CONSTRAINT IF EXISTS "enrollment_tokens_consumed_by_attempt_id_enrollment_attempts_id_fk";
--> statement-breakpoint
ALTER TABLE "enrollment_tokens" DROP CONSTRAINT IF EXISTS "enrollment_tokens_consumed_by_attempt_id_fkey";
--> statement-breakpoint
ALTER TABLE "enrollment_grants" DROP CONSTRAINT IF EXISTS "enrollment_grants_attempt_id_enrollment_attempts_id_fk";
--> statement-breakpoint
ALTER TABLE "enrollment_grants" DROP CONSTRAINT IF EXISTS "enrollment_grants_attempt_id_fkey";
--> statement-breakpoint
ALTER TABLE "enrolled_nodes" DROP CONSTRAINT IF EXISTS "enrolled_nodes_created_by_attempt_id_enrollment_attempts_id_fk";
--> statement-breakpoint
ALTER TABLE "enrolled_nodes" DROP CONSTRAINT IF EXISTS "enrolled_nodes_created_by_attempt_id_fkey";
