-- Effective migration-first PG order; 0008 appends the raw snapshot body.
-- Writers supply UUIDs and tagged clocks. Opaque text is not JSON.
CREATE TABLE installation_identity (
    id TEXT NOT NULL PRIMARY KEY CHECK (instr(id,char(0)) = 0),
    installation_id TEXT NOT NULL CHECK (instr(installation_id,char(0)) = 0 AND
        length(installation_id) = 36 AND substr(installation_id,9,1) = '-' AND substr(installation_id,14,1) = '-'
        AND substr(installation_id,19,1) = '-' AND substr(installation_id,24,1) = '-'
        AND length(replace(installation_id,'-','')) = 32 AND replace(installation_id,'-','') NOT GLOB '*[^0-9a-f]*'),
    node_id TEXT NOT NULL CHECK (instr(node_id,char(0)) = 0 AND
        length(node_id) = 36 AND substr(node_id,9,1) = '-' AND substr(node_id,14,1) = '-'
        AND substr(node_id,19,1) = '-' AND substr(node_id,24,1) = '-'
        AND length(replace(node_id,'-','')) = 32 AND replace(node_id,'-','') NOT GLOB '*[^0-9a-f]*'),
    role TEXT NOT NULL DEFAULT 'standalone-primary' CHECK (instr(role,char(0)) = 0 AND
        role IN ('standalone-primary','primary','primary-with-nodes','node','stale-primary')),
    leadership_generation INTEGER NOT NULL DEFAULT 1,
    latest_known_generation INTEGER NOT NULL DEFAULT 1,
    cluster_key_id TEXT CHECK (cluster_key_id IS NULL OR (instr(cluster_key_id,char(0)) = 0 AND
        length(cluster_key_id) = 36 AND substr(cluster_key_id,9,1) = '-' AND substr(cluster_key_id,14,1) = '-'
        AND substr(cluster_key_id,19,1) = '-' AND substr(cluster_key_id,24,1) = '-'
        AND length(replace(cluster_key_id,'-','')) = 32 AND replace(cluster_key_id,'-','') NOT GLOB '*[^0-9a-f]*')),
    updated_at TEXT COLLATE BINARY NOT NULL CHECK (instr(updated_at,char(0)) = 0 AND (
        updated_at IN ('0','2') OR (length(updated_at) = 31 AND substr(updated_at,1,1) = '1'
        AND substr(updated_at,22,1) = '.' AND substr(updated_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(updated_at,23,9) NOT GLOB '*[^0-9]*' AND substr(updated_at,2,20) <= '18446744073709551615')))
) STRICT;

CREATE TABLE cluster_keys (
    id TEXT NOT NULL PRIMARY KEY CHECK (instr(id,char(0)) = 0 AND
        length(id) = 36 AND substr(id,9,1) = '-' AND substr(id,14,1) = '-'
        AND substr(id,19,1) = '-' AND substr(id,24,1) = '-'
        AND length(replace(id,'-','')) = 32 AND replace(id,'-','') NOT GLOB '*[^0-9a-f]*'),
    purpose TEXT NOT NULL CHECK (instr(purpose,char(0)) = 0),
    wrapped_kek TEXT NOT NULL CHECK (instr(wrapped_kek,char(0)) = 0),
    wrapping_key_version INTEGER NOT NULL CHECK (wrapping_key_version BETWEEN -2147483648 AND 2147483647),
    created_at TEXT COLLATE BINARY NOT NULL CHECK (instr(created_at,char(0)) = 0 AND (
        created_at IN ('0','2') OR (length(created_at) = 31 AND substr(created_at,1,1) = '1'
        AND substr(created_at,22,1) = '.' AND substr(created_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(created_at,23,9) NOT GLOB '*[^0-9]*' AND substr(created_at,2,20) <= '18446744073709551615'))),
    retired_at TEXT COLLATE BINARY CHECK (retired_at IS NULL OR (instr(retired_at,char(0)) = 0 AND (
        retired_at IN ('0','2') OR (length(retired_at) = 31 AND substr(retired_at,1,1) = '1'
        AND substr(retired_at,22,1) = '.' AND substr(retired_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(retired_at,23,9) NOT GLOB '*[^0-9]*' AND substr(retired_at,2,20) <= '18446744073709551615'))))
) STRICT;

CREATE TABLE node_state (
    id TEXT NOT NULL PRIMARY KEY CHECK (instr(id,char(0)) = 0),
    enrolled_at TEXT COLLATE BINARY CHECK (enrolled_at IS NULL OR (instr(enrolled_at,char(0)) = 0 AND (
        enrolled_at IN ('0','2') OR (length(enrolled_at) = 31 AND substr(enrolled_at,1,1) = '1'
        AND substr(enrolled_at,22,1) = '.' AND substr(enrolled_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(enrolled_at,23,9) NOT GLOB '*[^0-9]*' AND substr(enrolled_at,2,20) <= '18446744073709551615')))),
    enrollment_token_hash TEXT CHECK (enrollment_token_hash IS NULL OR instr(enrollment_token_hash,char(0)) = 0),
    enrollment_primary_id TEXT CHECK (enrollment_primary_id IS NULL OR (instr(enrollment_primary_id,char(0)) = 0 AND
        length(enrollment_primary_id) = 36 AND substr(enrollment_primary_id,9,1) = '-' AND substr(enrollment_primary_id,14,1) = '-'
        AND substr(enrollment_primary_id,19,1) = '-' AND substr(enrollment_primary_id,24,1) = '-'
        AND length(replace(enrollment_primary_id,'-','')) = 32 AND replace(enrollment_primary_id,'-','') NOT GLOB '*[^0-9a-f]*')),
    last_seen_at TEXT COLLATE BINARY CHECK (last_seen_at IS NULL OR (instr(last_seen_at,char(0)) = 0 AND (
        last_seen_at IN ('0','2') OR (length(last_seen_at) = 31 AND substr(last_seen_at,1,1) = '1'
        AND substr(last_seen_at,22,1) = '.' AND substr(last_seen_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(last_seen_at,23,9) NOT GLOB '*[^0-9]*' AND substr(last_seen_at,2,20) <= '18446744073709551615')))),
    last_applied_snapshot_id TEXT CHECK (last_applied_snapshot_id IS NULL OR (instr(last_applied_snapshot_id,char(0)) = 0 AND
        length(last_applied_snapshot_id) = 36 AND substr(last_applied_snapshot_id,9,1) = '-' AND substr(last_applied_snapshot_id,14,1) = '-'
        AND substr(last_applied_snapshot_id,19,1) = '-' AND substr(last_applied_snapshot_id,24,1) = '-'
        AND length(replace(last_applied_snapshot_id,'-','')) = 32 AND replace(last_applied_snapshot_id,'-','') NOT GLOB '*[^0-9a-f]*')),
    enrollment_attempt_id TEXT CHECK (enrollment_attempt_id IS NULL OR (instr(enrollment_attempt_id,char(0)) = 0 AND
        length(enrollment_attempt_id) = 36 AND substr(enrollment_attempt_id,9,1) = '-' AND substr(enrollment_attempt_id,14,1) = '-'
        AND substr(enrollment_attempt_id,19,1) = '-' AND substr(enrollment_attempt_id,24,1) = '-'
        AND length(replace(enrollment_attempt_id,'-','')) = 32 AND replace(enrollment_attempt_id,'-','') NOT GLOB '*[^0-9a-f]*')),
    primary_url TEXT CHECK (primary_url IS NULL OR instr(primary_url,char(0)) = 0),
    primary_installation_id TEXT CHECK (primary_installation_id IS NULL OR (instr(primary_installation_id,char(0)) = 0 AND
        length(primary_installation_id) = 36 AND substr(primary_installation_id,9,1) = '-' AND substr(primary_installation_id,14,1) = '-'
        AND substr(primary_installation_id,19,1) = '-' AND substr(primary_installation_id,24,1) = '-'
        AND length(replace(primary_installation_id,'-','')) = 32 AND replace(primary_installation_id,'-','') NOT GLOB '*[^0-9a-f]*')),
    primary_tls_spki_sha256 TEXT CHECK (primary_tls_spki_sha256 IS NULL OR instr(primary_tls_spki_sha256,char(0)) = 0),
    credential_id TEXT CHECK (credential_id IS NULL OR (instr(credential_id,char(0)) = 0 AND
        length(credential_id) = 36 AND substr(credential_id,9,1) = '-' AND substr(credential_id,14,1) = '-'
        AND substr(credential_id,19,1) = '-' AND substr(credential_id,24,1) = '-'
        AND length(replace(credential_id,'-','')) = 32 AND replace(credential_id,'-','') NOT GLOB '*[^0-9a-f]*')),
    sync_enabled INTEGER NOT NULL DEFAULT 0 CHECK (sync_enabled IN (0,1)),
    last_attempt_at TEXT COLLATE BINARY CHECK (last_attempt_at IS NULL OR (instr(last_attempt_at,char(0)) = 0 AND (
        last_attempt_at IN ('0','2') OR (length(last_attempt_at) = 31 AND substr(last_attempt_at,1,1) = '1'
        AND substr(last_attempt_at,22,1) = '.' AND substr(last_attempt_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(last_attempt_at,23,9) NOT GLOB '*[^0-9]*' AND substr(last_attempt_at,2,20) <= '18446744073709551615')))),
    last_success_at TEXT COLLATE BINARY CHECK (last_success_at IS NULL OR (instr(last_success_at,char(0)) = 0 AND (
        last_success_at IN ('0','2') OR (length(last_success_at) = 31 AND substr(last_success_at,1,1) = '1'
        AND substr(last_success_at,22,1) = '.' AND substr(last_success_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(last_success_at,23,9) NOT GLOB '*[^0-9]*' AND substr(last_success_at,2,20) <= '18446744073709551615')))),
    consecutive_failures INTEGER NOT NULL DEFAULT 0 CHECK (consecutive_failures BETWEEN -2147483648 AND 2147483647),
    next_attempt_at TEXT COLLATE BINARY CHECK (next_attempt_at IS NULL OR (instr(next_attempt_at,char(0)) = 0 AND (
        next_attempt_at IN ('0','2') OR (length(next_attempt_at) = 31 AND substr(next_attempt_at,1,1) = '1'
        AND substr(next_attempt_at,22,1) = '.' AND substr(next_attempt_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(next_attempt_at,23,9) NOT GLOB '*[^0-9]*' AND substr(next_attempt_at,2,20) <= '18446744073709551615')))),
    last_error_code TEXT CHECK (last_error_code IS NULL OR instr(last_error_code,char(0)) = 0),
    updated_at TEXT COLLATE BINARY NOT NULL CHECK (instr(updated_at,char(0)) = 0 AND (
        updated_at IN ('0','2') OR (length(updated_at) = 31 AND substr(updated_at,1,1) = '1'
        AND substr(updated_at,22,1) = '.' AND substr(updated_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(updated_at,23,9) NOT GLOB '*[^0-9]*' AND substr(updated_at,2,20) <= '18446744073709551615')))
) STRICT;
-- Go's unconditional indexes apply even when its ADD IF NOT EXISTS refs are no-ops.
CREATE UNIQUE INDEX node_state_enrollment_attempt_idx ON node_state(enrollment_attempt_id);
CREATE UNIQUE INDEX node_state_credential_idx ON node_state(credential_id);

CREATE TABLE applied_snapshots (
    id TEXT NOT NULL PRIMARY KEY CHECK (instr(id,char(0)) = 0 AND
        length(id) = 36 AND substr(id,9,1) = '-' AND substr(id,14,1) = '-'
        AND substr(id,19,1) = '-' AND substr(id,24,1) = '-'
        AND length(replace(id,'-','')) = 32 AND replace(id,'-','') NOT GLOB '*[^0-9a-f]*'),
    source_primary_id TEXT NOT NULL CHECK (instr(source_primary_id,char(0)) = 0 AND
        length(source_primary_id) = 36 AND substr(source_primary_id,9,1) = '-' AND substr(source_primary_id,14,1) = '-'
        AND substr(source_primary_id,19,1) = '-' AND substr(source_primary_id,24,1) = '-'
        AND length(replace(source_primary_id,'-','')) = 32 AND replace(source_primary_id,'-','') NOT GLOB '*[^0-9a-f]*'),
    leadership_generation INTEGER NOT NULL,
    snapshot_version INTEGER NOT NULL CHECK (snapshot_version BETWEEN -2147483648 AND 2147483647),
    replication_version INTEGER NOT NULL CHECK (replication_version BETWEEN -2147483648 AND 2147483647),
    content_hash TEXT NOT NULL CHECK (instr(content_hash,char(0)) = 0),
    revision_id TEXT REFERENCES config_revisions(id) ON DELETE NO ACTION ON UPDATE NO ACTION CHECK (revision_id IS NULL OR (
        instr(revision_id,char(0)) = 0 AND length(revision_id) = 36 AND substr(revision_id,9,1) = '-' AND substr(revision_id,14,1) = '-'
        AND substr(revision_id,19,1) = '-' AND substr(revision_id,24,1) = '-'
        AND length(replace(revision_id,'-','')) = 32 AND replace(revision_id,'-','') NOT GLOB '*[^0-9a-f]*')),
    status TEXT NOT NULL DEFAULT 'applied' CHECK (instr(status,char(0)) = 0),
    apply_job_id TEXT REFERENCES apply_jobs(id) ON DELETE NO ACTION ON UPDATE NO ACTION CHECK (apply_job_id IS NULL OR (
        instr(apply_job_id,char(0)) = 0 AND length(apply_job_id) = 36 AND substr(apply_job_id,9,1) = '-' AND substr(apply_job_id,14,1) = '-'
        AND substr(apply_job_id,19,1) = '-' AND substr(apply_job_id,24,1) = '-'
        AND length(replace(apply_job_id,'-','')) = 32 AND replace(apply_job_id,'-','') NOT GLOB '*[^0-9a-f]*')),
    failure_code TEXT CHECK (failure_code IS NULL OR instr(failure_code,char(0)) = 0),
    applied_at TEXT COLLATE BINARY NOT NULL CHECK (instr(applied_at,char(0)) = 0 AND (
        applied_at IN ('0','2') OR (length(applied_at) = 31 AND substr(applied_at,1,1) = '1'
        AND substr(applied_at,22,1) = '.' AND substr(applied_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(applied_at,23,9) NOT GLOB '*[^0-9]*' AND substr(applied_at,2,20) <= '18446744073709551615'))),
    discarded_at TEXT COLLATE BINARY CHECK (discarded_at IS NULL OR (instr(discarded_at,char(0)) = 0 AND (
        discarded_at IN ('0','2') OR (length(discarded_at) = 31 AND substr(discarded_at,1,1) = '1'
        AND substr(discarded_at,22,1) = '.' AND substr(discarded_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(discarded_at,23,9) NOT GLOB '*[^0-9]*' AND substr(discarded_at,2,20) <= '18446744073709551615')))),
    snapshot_body BLOB,
    CONSTRAINT applied_snapshots_status_check CHECK (status IN ('pending','applied','rejected','rolled-back','archived'))
) STRICT;
CREATE UNIQUE INDEX applied_snapshots_content_hash_idx ON applied_snapshots(content_hash);

CREATE TABLE standalone_archives (
    id TEXT NOT NULL PRIMARY KEY CHECK (instr(id,char(0)) = 0 AND
        length(id) = 36 AND substr(id,9,1) = '-' AND substr(id,14,1) = '-'
        AND substr(id,19,1) = '-' AND substr(id,24,1) = '-'
        AND length(replace(id,'-','')) = 32 AND replace(id,'-','') NOT GLOB '*[^0-9a-f]*'),
    snapshot_content_hash TEXT CHECK (snapshot_content_hash IS NULL OR instr(snapshot_content_hash,char(0)) = 0),
    revision_id TEXT REFERENCES config_revisions(id) ON DELETE NO ACTION ON UPDATE NO ACTION CHECK (revision_id IS NULL OR (
        instr(revision_id,char(0)) = 0 AND length(revision_id) = 36 AND substr(revision_id,9,1) = '-' AND substr(revision_id,14,1) = '-'
        AND substr(revision_id,19,1) = '-' AND substr(revision_id,24,1) = '-'
        AND length(replace(revision_id,'-','')) = 32 AND replace(revision_id,'-','') NOT GLOB '*[^0-9a-f]*')),
    archive_blob TEXT NOT NULL CHECK (instr(archive_blob,char(0)) = 0),
    capture_reason TEXT NOT NULL CHECK (instr(capture_reason,char(0)) = 0),
    captured_at TEXT COLLATE BINARY NOT NULL CHECK (instr(captured_at,char(0)) = 0 AND (
        captured_at IN ('0','2') OR (length(captured_at) = 31 AND substr(captured_at,1,1) = '1'
        AND substr(captured_at,22,1) = '.' AND substr(captured_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(captured_at,23,9) NOT GLOB '*[^0-9]*' AND substr(captured_at,2,20) <= '18446744073709551615'))),
    retention_started_at TEXT COLLATE BINARY CHECK (retention_started_at IS NULL OR (instr(retention_started_at,char(0)) = 0 AND (
        retention_started_at IN ('0','2') OR (length(retention_started_at) = 31 AND substr(retention_started_at,1,1) = '1'
        AND substr(retention_started_at,22,1) = '.' AND substr(retention_started_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(retention_started_at,23,9) NOT GLOB '*[^0-9]*' AND substr(retention_started_at,2,20) <= '18446744073709551615')))),
    expires_at TEXT COLLATE BINARY CHECK (expires_at IS NULL OR (instr(expires_at,char(0)) = 0 AND (
        expires_at IN ('0','2') OR (length(expires_at) = 31 AND substr(expires_at,1,1) = '1'
        AND substr(expires_at,22,1) = '.' AND substr(expires_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(expires_at,23,9) NOT GLOB '*[^0-9]*' AND substr(expires_at,2,20) <= '18446744073709551615')))),
    restoration_eligible INTEGER NOT NULL DEFAULT 1 CHECK (restoration_eligible IN (0,1)),
    created_at TEXT COLLATE BINARY NOT NULL CHECK (instr(created_at,char(0)) = 0 AND (
        created_at IN ('0','2') OR (length(created_at) = 31 AND substr(created_at,1,1) = '1'
        AND substr(created_at,22,1) = '.' AND substr(created_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(created_at,23,9) NOT GLOB '*[^0-9]*' AND substr(created_at,2,20) <= '18446744073709551615')))
) STRICT;
CREATE INDEX standalone_archives_expiry_idx ON standalone_archives(expires_at);
