CREATE TABLE IF NOT EXISTS "internal_ca_enrollment_state" (
	"id" text PRIMARY KEY,
	"established_at" timestamp with time zone NOT NULL
);
--> statement-breakpoint
ALTER TABLE "internal_ca" ADD COLUMN "enrollment_certificate_pem" text;
--> statement-breakpoint
ALTER TABLE "internal_ca" ADD COLUMN "enrollment_key_secret_id" uuid;
--> statement-breakpoint
DO $$ BEGIN
 ALTER TABLE "internal_ca" ADD CONSTRAINT "internal_ca_enrollment_key_secret_id_secrets_id_fk" FOREIGN KEY ("enrollment_key_secret_id") REFERENCES "public"."secrets"("id") ON DELETE no action ON UPDATE no action;
EXCEPTION
 WHEN duplicate_object THEN null;
END $$;
