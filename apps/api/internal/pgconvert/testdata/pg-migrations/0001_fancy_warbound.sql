ALTER TYPE "public"."proxycore_certificate_issuer" ADD VALUE 'uploaded' BEFORE 'letsencrypt';--> statement-breakpoint
ALTER TABLE "certificates" ADD COLUMN "failure_reason" text;