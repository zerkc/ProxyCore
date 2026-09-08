ALTER TABLE "users" ADD COLUMN IF NOT EXISTS "password_change_required" boolean DEFAULT false NOT NULL;
