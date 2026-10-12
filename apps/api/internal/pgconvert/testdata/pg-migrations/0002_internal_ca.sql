CREATE TABLE IF NOT EXISTS "internal_ca" (
	"id" text PRIMARY KEY,
	"certificate_pem" text NOT NULL,
	"key_secret_id" uuid NOT NULL,
	"created_at" timestamp with time zone DEFAULT now() NOT NULL,
	"updated_at" timestamp with time zone DEFAULT now() NOT NULL
);
--> statement-breakpoint
DO $$ BEGIN
 ALTER TABLE "internal_ca" ADD CONSTRAINT "internal_ca_key_secret_id_secrets_id_fk" FOREIGN KEY ("key_secret_id") REFERENCES "public"."secrets"("id") ON DELETE no action ON UPDATE no action;
EXCEPTION
 WHEN duplicate_object THEN null;
END $$;
