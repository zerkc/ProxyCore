-- Effective migration-first enrollment schema through PG 0007.
-- UUID and timestamp defaults are explicit writer responsibilities.
-- Opaque bootstrap payloads, keys and digests are text, not JSON.
CREATE TABLE enrollment_attempts (
    id TEXT NOT NULL PRIMARY KEY CHECK (instr(id,char(0)) = 0 AND
        length(id) = 36 AND substr(id,9,1) = '-' AND substr(id,14,1) = '-'
        AND substr(id,19,1) = '-' AND substr(id,24,1) = '-'
        AND length(replace(id,'-','')) = 32 AND replace(id,'-','') NOT GLOB '*[^0-9a-f]*'),
    state TEXT NOT NULL DEFAULT 'draft' CHECK (instr(state,char(0)) = 0),
    primary_url TEXT NOT NULL CHECK (instr(primary_url,char(0)) = 0),
    expected_primary_id TEXT CHECK (expected_primary_id IS NULL OR (instr(expected_primary_id,char(0)) = 0 AND
        length(expected_primary_id) = 36 AND substr(expected_primary_id,9,1) = '-' AND substr(expected_primary_id,14,1) = '-'
        AND substr(expected_primary_id,19,1) = '-' AND substr(expected_primary_id,24,1) = '-'
        AND length(replace(expected_primary_id,'-','')) = 32 AND replace(expected_primary_id,'-','') NOT GLOB '*[^0-9a-f]*')),
    verified_primary_id TEXT CHECK (verified_primary_id IS NULL OR (instr(verified_primary_id,char(0)) = 0 AND
        length(verified_primary_id) = 36 AND substr(verified_primary_id,9,1) = '-' AND substr(verified_primary_id,14,1) = '-'
        AND substr(verified_primary_id,19,1) = '-' AND substr(verified_primary_id,24,1) = '-'
        AND length(replace(verified_primary_id,'-','')) = 32 AND replace(verified_primary_id,'-','') NOT GLOB '*[^0-9a-f]*')),
    verified_primary_node_id TEXT CHECK (verified_primary_node_id IS NULL OR (instr(verified_primary_node_id,char(0)) = 0 AND
        length(verified_primary_node_id) = 36 AND substr(verified_primary_node_id,9,1) = '-' AND substr(verified_primary_node_id,14,1) = '-'
        AND substr(verified_primary_node_id,19,1) = '-' AND substr(verified_primary_node_id,24,1) = '-'
        AND length(replace(verified_primary_node_id,'-','')) = 32 AND replace(verified_primary_node_id,'-','') NOT GLOB '*[^0-9a-f]*')),
    verified_leadership_generation INTEGER,
    verified_primary_tls_spki_sha256 TEXT CHECK (verified_primary_tls_spki_sha256 IS NULL OR instr(verified_primary_tls_spki_sha256,char(0)) = 0),
    verified_primary_ca_fingerprint TEXT CHECK (verified_primary_ca_fingerprint IS NULL OR instr(verified_primary_ca_fingerprint,char(0)) = 0),
    preview_digest TEXT CHECK (preview_digest IS NULL OR instr(preview_digest,char(0)) = 0),
    local_node_ip TEXT NOT NULL CHECK (instr(local_node_ip,char(0)) = 0),
    archive_id TEXT REFERENCES standalone_archives(id) ON DELETE NO ACTION ON UPDATE NO ACTION CHECK (archive_id IS NULL OR (
        instr(archive_id,char(0)) = 0 AND length(archive_id) = 36 AND substr(archive_id,9,1) = '-' AND substr(archive_id,14,1) = '-'
        AND substr(archive_id,19,1) = '-' AND substr(archive_id,24,1) = '-'
        AND length(replace(archive_id,'-','')) = 32 AND replace(archive_id,'-','') NOT GLOB '*[^0-9a-f]*')),
    ephemeral_private_key_wrapped TEXT NOT NULL CHECK (instr(ephemeral_private_key_wrapped,char(0)) = 0),
    bootstrap_payload TEXT CHECK (bootstrap_payload IS NULL OR instr(bootstrap_payload,char(0)) = 0),
    node_credential_secret_id TEXT REFERENCES secrets(id) ON DELETE NO ACTION ON UPDATE NO ACTION CHECK (node_credential_secret_id IS NULL OR (
        instr(node_credential_secret_id,char(0)) = 0 AND length(node_credential_secret_id) = 36 AND substr(node_credential_secret_id,9,1) = '-' AND substr(node_credential_secret_id,14,1) = '-'
        AND substr(node_credential_secret_id,19,1) = '-' AND substr(node_credential_secret_id,24,1) = '-'
        AND length(replace(node_credential_secret_id,'-','')) = 32 AND replace(node_credential_secret_id,'-','') NOT GLOB '*[^0-9a-f]*')),
    cluster_key_id TEXT REFERENCES cluster_keys(id) ON DELETE NO ACTION ON UPDATE NO ACTION CHECK (cluster_key_id IS NULL OR (
        instr(cluster_key_id,char(0)) = 0 AND length(cluster_key_id) = 36 AND substr(cluster_key_id,9,1) = '-' AND substr(cluster_key_id,14,1) = '-'
        AND substr(cluster_key_id,19,1) = '-' AND substr(cluster_key_id,24,1) = '-'
        AND length(replace(cluster_key_id,'-','')) = 32 AND replace(cluster_key_id,'-','') NOT GLOB '*[^0-9a-f]*')),
    initial_snapshot_hash TEXT CHECK (initial_snapshot_hash IS NULL OR instr(initial_snapshot_hash,char(0)) = 0),
    initial_snapshot_revision_id TEXT REFERENCES config_revisions(id) ON DELETE NO ACTION ON UPDATE NO ACTION CHECK (initial_snapshot_revision_id IS NULL OR (
        instr(initial_snapshot_revision_id,char(0)) = 0 AND length(initial_snapshot_revision_id) = 36 AND substr(initial_snapshot_revision_id,9,1) = '-' AND substr(initial_snapshot_revision_id,14,1) = '-'
        AND substr(initial_snapshot_revision_id,19,1) = '-' AND substr(initial_snapshot_revision_id,24,1) = '-'
        AND length(replace(initial_snapshot_revision_id,'-','')) = 32 AND replace(initial_snapshot_revision_id,'-','') NOT GLOB '*[^0-9a-f]*')),
    initial_apply_job_id TEXT REFERENCES apply_jobs(id) ON DELETE NO ACTION ON UPDATE NO ACTION CHECK (initial_apply_job_id IS NULL OR (
        instr(initial_apply_job_id,char(0)) = 0 AND length(initial_apply_job_id) = 36 AND substr(initial_apply_job_id,9,1) = '-' AND substr(initial_apply_job_id,14,1) = '-'
        AND substr(initial_apply_job_id,19,1) = '-' AND substr(initial_apply_job_id,24,1) = '-'
        AND length(replace(initial_apply_job_id,'-','')) = 32 AND replace(initial_apply_job_id,'-','') NOT GLOB '*[^0-9a-f]*')),
    failure_code TEXT CHECK (failure_code IS NULL OR instr(failure_code,char(0)) = 0),
    confirmed_at TEXT COLLATE BINARY CHECK (confirmed_at IS NULL OR (instr(confirmed_at,char(0)) = 0 AND (
        confirmed_at IN ('0','2') OR (length(confirmed_at) = 31 AND substr(confirmed_at,1,1) = '1'
        AND substr(confirmed_at,22,1) = '.' AND substr(confirmed_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(confirmed_at,23,9) NOT GLOB '*[^0-9]*' AND substr(confirmed_at,2,20) <= '18446744073709551615')))),
    created_at TEXT COLLATE BINARY NOT NULL CHECK (instr(created_at,char(0)) = 0 AND (
        created_at IN ('0','2') OR (length(created_at) = 31 AND substr(created_at,1,1) = '1'
        AND substr(created_at,22,1) = '.' AND substr(created_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(created_at,23,9) NOT GLOB '*[^0-9]*' AND substr(created_at,2,20) <= '18446744073709551615'))),
    updated_at TEXT COLLATE BINARY NOT NULL CHECK (instr(updated_at,char(0)) = 0 AND (
        updated_at IN ('0','2') OR (length(updated_at) = 31 AND substr(updated_at,1,1) = '1'
        AND substr(updated_at,22,1) = '.' AND substr(updated_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(updated_at,23,9) NOT GLOB '*[^0-9]*' AND substr(updated_at,2,20) <= '18446744073709551615'))),
    CONSTRAINT enrollment_attempts_state_check CHECK (state IN ('draft','verified','confirmed','exchanged','archived','initial-apply-pending','committed','cancelled','recoverable','failed'))
) STRICT;
CREATE UNIQUE INDEX enrollment_attempts_one_active_idx ON enrollment_attempts((1)) WHERE state NOT IN ('committed','cancelled','failed');

CREATE TABLE enrollment_tokens (
    id TEXT NOT NULL PRIMARY KEY CHECK (instr(id,char(0)) = 0 AND
        length(id) = 36 AND substr(id,9,1) = '-' AND substr(id,14,1) = '-'
        AND substr(id,19,1) = '-' AND substr(id,24,1) = '-'
        AND length(replace(id,'-','')) = 32 AND replace(id,'-','') NOT GLOB '*[^0-9a-f]*'),
    token_selector TEXT NOT NULL CHECK (instr(token_selector,char(0)) = 0),
    token_hash TEXT NOT NULL CHECK (instr(token_hash,char(0)) = 0),
    hash_version TEXT NOT NULL CHECK (instr(hash_version,char(0)) = 0),
    created_by_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE NO ACTION ON UPDATE NO ACTION CHECK (instr(created_by_user_id,char(0)) = 0 AND
        length(created_by_user_id) = 36 AND substr(created_by_user_id,9,1) = '-' AND substr(created_by_user_id,14,1) = '-'
        AND substr(created_by_user_id,19,1) = '-' AND substr(created_by_user_id,24,1) = '-'
        AND length(replace(created_by_user_id,'-','')) = 32 AND replace(created_by_user_id,'-','') NOT GLOB '*[^0-9a-f]*'),
    created_at TEXT COLLATE BINARY NOT NULL CHECK (instr(created_at,char(0)) = 0 AND (
        created_at IN ('0','2') OR (length(created_at) = 31 AND substr(created_at,1,1) = '1'
        AND substr(created_at,22,1) = '.' AND substr(created_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(created_at,23,9) NOT GLOB '*[^0-9]*' AND substr(created_at,2,20) <= '18446744073709551615'))),
    expires_at TEXT COLLATE BINARY NOT NULL CHECK (instr(expires_at,char(0)) = 0 AND (
        expires_at IN ('0','2') OR (length(expires_at) = 31 AND substr(expires_at,1,1) = '1'
        AND substr(expires_at,22,1) = '.' AND substr(expires_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(expires_at,23,9) NOT GLOB '*[^0-9]*' AND substr(expires_at,2,20) <= '18446744073709551615'))),
    consumed_at TEXT COLLATE BINARY CHECK (consumed_at IS NULL OR (instr(consumed_at,char(0)) = 0 AND (
        consumed_at IN ('0','2') OR (length(consumed_at) = 31 AND substr(consumed_at,1,1) = '1'
        AND substr(consumed_at,22,1) = '.' AND substr(consumed_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(consumed_at,23,9) NOT GLOB '*[^0-9]*' AND substr(consumed_at,2,20) <= '18446744073709551615')))),
    consumed_by_attempt_id TEXT CHECK (consumed_by_attempt_id IS NULL OR (instr(consumed_by_attempt_id,char(0)) = 0 AND
        length(consumed_by_attempt_id) = 36 AND substr(consumed_by_attempt_id,9,1) = '-' AND substr(consumed_by_attempt_id,14,1) = '-'
        AND substr(consumed_by_attempt_id,19,1) = '-' AND substr(consumed_by_attempt_id,24,1) = '-'
        AND length(replace(consumed_by_attempt_id,'-','')) = 32 AND replace(consumed_by_attempt_id,'-','') NOT GLOB '*[^0-9a-f]*')),
    revoked_at TEXT COLLATE BINARY CHECK (revoked_at IS NULL OR (instr(revoked_at,char(0)) = 0 AND (
        revoked_at IN ('0','2') OR (length(revoked_at) = 31 AND substr(revoked_at,1,1) = '1'
        AND substr(revoked_at,22,1) = '.' AND substr(revoked_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(revoked_at,23,9) NOT GLOB '*[^0-9]*' AND substr(revoked_at,2,20) <= '18446744073709551615')))),
    CONSTRAINT enrollment_tokens_expiry_check CHECK (expires_at > created_at)
) STRICT;
CREATE UNIQUE INDEX enrollment_tokens_selector_idx ON enrollment_tokens(token_selector);

CREATE TABLE node_credentials (
    id TEXT NOT NULL PRIMARY KEY CHECK (instr(id,char(0)) = 0 AND
        length(id) = 36 AND substr(id,9,1) = '-' AND substr(id,14,1) = '-'
        AND substr(id,19,1) = '-' AND substr(id,24,1) = '-'
        AND length(replace(id,'-','')) = 32 AND replace(id,'-','') NOT GLOB '*[^0-9a-f]*'),
    node_id TEXT NOT NULL CHECK (instr(node_id,char(0)) = 0 AND
        length(node_id) = 36 AND substr(node_id,9,1) = '-' AND substr(node_id,14,1) = '-'
        AND substr(node_id,19,1) = '-' AND substr(node_id,24,1) = '-'
        AND length(replace(node_id,'-','')) = 32 AND replace(node_id,'-','') NOT GLOB '*[^0-9a-f]*'),
    credential_hash TEXT NOT NULL CHECK (instr(credential_hash,char(0)) = 0),
    hash_version TEXT NOT NULL CHECK (instr(hash_version,char(0)) = 0),
    created_at TEXT COLLATE BINARY NOT NULL CHECK (instr(created_at,char(0)) = 0 AND (
        created_at IN ('0','2') OR (length(created_at) = 31 AND substr(created_at,1,1) = '1'
        AND substr(created_at,22,1) = '.' AND substr(created_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(created_at,23,9) NOT GLOB '*[^0-9]*' AND substr(created_at,2,20) <= '18446744073709551615'))),
    last_authenticated_at TEXT COLLATE BINARY CHECK (last_authenticated_at IS NULL OR (instr(last_authenticated_at,char(0)) = 0 AND (
        last_authenticated_at IN ('0','2') OR (length(last_authenticated_at) = 31 AND substr(last_authenticated_at,1,1) = '1'
        AND substr(last_authenticated_at,22,1) = '.' AND substr(last_authenticated_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(last_authenticated_at,23,9) NOT GLOB '*[^0-9]*' AND substr(last_authenticated_at,2,20) <= '18446744073709551615')))),
    revoked_at TEXT COLLATE BINARY CHECK (revoked_at IS NULL OR (instr(revoked_at,char(0)) = 0 AND (
        revoked_at IN ('0','2') OR (length(revoked_at) = 31 AND substr(revoked_at,1,1) = '1'
        AND substr(revoked_at,22,1) = '.' AND substr(revoked_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(revoked_at,23,9) NOT GLOB '*[^0-9]*' AND substr(revoked_at,2,20) <= '18446744073709551615'))))
) STRICT;
CREATE UNIQUE INDEX node_credentials_node_idx ON node_credentials(node_id);

CREATE TABLE enrolled_nodes (
    node_id TEXT NOT NULL PRIMARY KEY CHECK (instr(node_id,char(0)) = 0 AND
        length(node_id) = 36 AND substr(node_id,9,1) = '-' AND substr(node_id,14,1) = '-'
        AND substr(node_id,19,1) = '-' AND substr(node_id,24,1) = '-'
        AND length(replace(node_id,'-','')) = 32 AND replace(node_id,'-','') NOT GLOB '*[^0-9a-f]*'),
    installation_id TEXT NOT NULL CHECK (instr(installation_id,char(0)) = 0 AND
        length(installation_id) = 36 AND substr(installation_id,9,1) = '-' AND substr(installation_id,14,1) = '-'
        AND substr(installation_id,19,1) = '-' AND substr(installation_id,24,1) = '-'
        AND length(replace(installation_id,'-','')) = 32 AND replace(installation_id,'-','') NOT GLOB '*[^0-9a-f]*'),
    primary_id TEXT NOT NULL CHECK (instr(primary_id,char(0)) = 0 AND
        length(primary_id) = 36 AND substr(primary_id,9,1) = '-' AND substr(primary_id,14,1) = '-'
        AND substr(primary_id,19,1) = '-' AND substr(primary_id,24,1) = '-'
        AND length(replace(primary_id,'-','')) = 32 AND replace(primary_id,'-','') NOT GLOB '*[^0-9a-f]*'),
    display_name TEXT CHECK (display_name IS NULL OR instr(display_name,char(0)) = 0),
    credential_id TEXT NOT NULL REFERENCES node_credentials(id) ON DELETE NO ACTION ON UPDATE NO ACTION CHECK (instr(credential_id,char(0)) = 0 AND
        length(credential_id) = 36 AND substr(credential_id,9,1) = '-' AND substr(credential_id,14,1) = '-'
        AND substr(credential_id,19,1) = '-' AND substr(credential_id,24,1) = '-'
        AND length(replace(credential_id,'-','')) = 32 AND replace(credential_id,'-','') NOT GLOB '*[^0-9a-f]*'),
    enrolled_at TEXT COLLATE BINARY NOT NULL CHECK (instr(enrolled_at,char(0)) = 0 AND (
        enrolled_at IN ('0','2') OR (length(enrolled_at) = 31 AND substr(enrolled_at,1,1) = '1'
        AND substr(enrolled_at,22,1) = '.' AND substr(enrolled_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(enrolled_at,23,9) NOT GLOB '*[^0-9]*' AND substr(enrolled_at,2,20) <= '18446744073709551615'))),
    revoked_at TEXT COLLATE BINARY CHECK (revoked_at IS NULL OR (instr(revoked_at,char(0)) = 0 AND (
        revoked_at IN ('0','2') OR (length(revoked_at) = 31 AND substr(revoked_at,1,1) = '1'
        AND substr(revoked_at,22,1) = '.' AND substr(revoked_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(revoked_at,23,9) NOT GLOB '*[^0-9]*' AND substr(revoked_at,2,20) <= '18446744073709551615')))),
    created_by_attempt_id TEXT NOT NULL CHECK (instr(created_by_attempt_id,char(0)) = 0 AND
        length(created_by_attempt_id) = 36 AND substr(created_by_attempt_id,9,1) = '-' AND substr(created_by_attempt_id,14,1) = '-'
        AND substr(created_by_attempt_id,19,1) = '-' AND substr(created_by_attempt_id,24,1) = '-'
        AND length(replace(created_by_attempt_id,'-','')) = 32 AND replace(created_by_attempt_id,'-','') NOT GLOB '*[^0-9a-f]*')
) STRICT;
CREATE UNIQUE INDEX enrolled_nodes_installation_idx ON enrolled_nodes(installation_id);
CREATE UNIQUE INDEX enrolled_nodes_credential_idx ON enrolled_nodes(credential_id);
CREATE UNIQUE INDEX enrolled_nodes_attempt_unique ON enrolled_nodes(created_by_attempt_id);

CREATE TABLE enrollment_grants (
    attempt_id TEXT NOT NULL PRIMARY KEY CHECK (instr(attempt_id,char(0)) = 0 AND
        length(attempt_id) = 36 AND substr(attempt_id,9,1) = '-' AND substr(attempt_id,14,1) = '-'
        AND substr(attempt_id,19,1) = '-' AND substr(attempt_id,24,1) = '-'
        AND length(replace(attempt_id,'-','')) = 32 AND replace(attempt_id,'-','') NOT GLOB '*[^0-9a-f]*'),
    token_id TEXT NOT NULL REFERENCES enrollment_tokens(id) ON DELETE NO ACTION ON UPDATE NO ACTION CHECK (instr(token_id,char(0)) = 0 AND
        length(token_id) = 36 AND substr(token_id,9,1) = '-' AND substr(token_id,14,1) = '-'
        AND substr(token_id,19,1) = '-' AND substr(token_id,24,1) = '-'
        AND length(replace(token_id,'-','')) = 32 AND replace(token_id,'-','') NOT GLOB '*[^0-9a-f]*'),
    installation_id TEXT NOT NULL CHECK (instr(installation_id,char(0)) = 0 AND
        length(installation_id) = 36 AND substr(installation_id,9,1) = '-' AND substr(installation_id,14,1) = '-'
        AND substr(installation_id,19,1) = '-' AND substr(installation_id,24,1) = '-'
        AND length(replace(installation_id,'-','')) = 32 AND replace(installation_id,'-','') NOT GLOB '*[^0-9a-f]*'),
    node_id TEXT NOT NULL CHECK (instr(node_id,char(0)) = 0 AND
        length(node_id) = 36 AND substr(node_id,9,1) = '-' AND substr(node_id,14,1) = '-'
        AND substr(node_id,19,1) = '-' AND substr(node_id,24,1) = '-'
        AND length(replace(node_id,'-','')) = 32 AND replace(node_id,'-','') NOT GLOB '*[^0-9a-f]*'),
    primary_id TEXT NOT NULL CHECK (instr(primary_id,char(0)) = 0 AND
        length(primary_id) = 36 AND substr(primary_id,9,1) = '-' AND substr(primary_id,14,1) = '-'
        AND substr(primary_id,19,1) = '-' AND substr(primary_id,24,1) = '-'
        AND length(replace(primary_id,'-','')) = 32 AND replace(primary_id,'-','') NOT GLOB '*[^0-9a-f]*'),
    primary_generation INTEGER NOT NULL,
    node_ephemeral_public_key TEXT NOT NULL CHECK (instr(node_ephemeral_public_key,char(0)) = 0),
    sealed_bootstrap_payload TEXT NOT NULL CHECK (instr(sealed_bootstrap_payload,char(0)) = 0),
    payload_hash TEXT NOT NULL CHECK (instr(payload_hash,char(0)) = 0),
    created_at TEXT COLLATE BINARY NOT NULL CHECK (instr(created_at,char(0)) = 0 AND (
        created_at IN ('0','2') OR (length(created_at) = 31 AND substr(created_at,1,1) = '1'
        AND substr(created_at,22,1) = '.' AND substr(created_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(created_at,23,9) NOT GLOB '*[^0-9]*' AND substr(created_at,2,20) <= '18446744073709551615'))),
    expires_at TEXT COLLATE BINARY NOT NULL CHECK (instr(expires_at,char(0)) = 0 AND (
        expires_at IN ('0','2') OR (length(expires_at) = 31 AND substr(expires_at,1,1) = '1'
        AND substr(expires_at,22,1) = '.' AND substr(expires_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(expires_at,23,9) NOT GLOB '*[^0-9]*' AND substr(expires_at,2,20) <= '18446744073709551615'))),
    verified_preview_digest TEXT CHECK (verified_preview_digest IS NULL OR instr(verified_preview_digest,char(0)) = 0),
    CONSTRAINT enrollment_grants_token_id_unique UNIQUE (token_id),
    CONSTRAINT enrollment_grants_expiry_check CHECK (expires_at > created_at)
) STRICT;

CREATE TABLE node_snapshot_acks (
    node_id TEXT NOT NULL REFERENCES enrolled_nodes(node_id) ON DELETE NO ACTION ON UPDATE NO ACTION CHECK (instr(node_id,char(0)) = 0 AND
        length(node_id) = 36 AND substr(node_id,9,1) = '-' AND substr(node_id,14,1) = '-'
        AND substr(node_id,19,1) = '-' AND substr(node_id,24,1) = '-'
        AND length(replace(node_id,'-','')) = 32 AND replace(node_id,'-','') NOT GLOB '*[^0-9a-f]*'),
    content_hash TEXT NOT NULL CHECK (instr(content_hash,char(0)) = 0),
    snapshot_version INTEGER NOT NULL CHECK (snapshot_version BETWEEN -2147483648 AND 2147483647),
    replication_version INTEGER NOT NULL CHECK (replication_version BETWEEN -2147483648 AND 2147483647),
    revision_id TEXT NOT NULL REFERENCES config_revisions(id) ON DELETE NO ACTION ON UPDATE NO ACTION CHECK (instr(revision_id,char(0)) = 0 AND
        length(revision_id) = 36 AND substr(revision_id,9,1) = '-' AND substr(revision_id,14,1) = '-'
        AND substr(revision_id,19,1) = '-' AND substr(revision_id,24,1) = '-'
        AND length(replace(revision_id,'-','')) = 32 AND replace(revision_id,'-','') NOT GLOB '*[^0-9a-f]*'),
    leadership_generation INTEGER NOT NULL,
    applied_at TEXT COLLATE BINARY NOT NULL CHECK (instr(applied_at,char(0)) = 0 AND (
        applied_at IN ('0','2') OR (length(applied_at) = 31 AND substr(applied_at,1,1) = '1'
        AND substr(applied_at,22,1) = '.' AND substr(applied_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(applied_at,23,9) NOT GLOB '*[^0-9]*' AND substr(applied_at,2,20) <= '18446744073709551615'))),
    received_at TEXT COLLATE BINARY NOT NULL CHECK (instr(received_at,char(0)) = 0 AND (
        received_at IN ('0','2') OR (length(received_at) = 31 AND substr(received_at,1,1) = '1'
        AND substr(received_at,22,1) = '.' AND substr(received_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(received_at,23,9) NOT GLOB '*[^0-9]*' AND substr(received_at,2,20) <= '18446744073709551615'))),
    CONSTRAINT node_snapshot_acks_node_id_content_hash_pk PRIMARY KEY (node_id,content_hash)
) STRICT;

CREATE TABLE sync_attempts (
    id TEXT NOT NULL PRIMARY KEY CHECK (instr(id,char(0)) = 0 AND
        length(id) = 36 AND substr(id,9,1) = '-' AND substr(id,14,1) = '-'
        AND substr(id,19,1) = '-' AND substr(id,24,1) = '-'
        AND length(replace(id,'-','')) = 32 AND replace(id,'-','') NOT GLOB '*[^0-9a-f]*'),
    node_id TEXT NOT NULL CHECK (instr(node_id,char(0)) = 0 AND
        length(node_id) = 36 AND substr(node_id,9,1) = '-' AND substr(node_id,14,1) = '-'
        AND substr(node_id,19,1) = '-' AND substr(node_id,24,1) = '-'
        AND length(replace(node_id,'-','')) = 32 AND replace(node_id,'-','') NOT GLOB '*[^0-9a-f]*'),
    "trigger" TEXT NOT NULL CHECK (instr("trigger",char(0)) = 0),
    status TEXT NOT NULL DEFAULT 'queued' CHECK (instr(status,char(0)) = 0 AND status IN ('queued','running','current','applied','failed','pending-ack')),
    source_primary_id TEXT CHECK (source_primary_id IS NULL OR (instr(source_primary_id,char(0)) = 0 AND
        length(source_primary_id) = 36 AND substr(source_primary_id,9,1) = '-' AND substr(source_primary_id,14,1) = '-'
        AND substr(source_primary_id,19,1) = '-' AND substr(source_primary_id,24,1) = '-'
        AND length(replace(source_primary_id,'-','')) = 32 AND replace(source_primary_id,'-','') NOT GLOB '*[^0-9a-f]*')),
    leadership_generation INTEGER,
    snapshot_version INTEGER CHECK (snapshot_version IS NULL OR snapshot_version BETWEEN -2147483648 AND 2147483647),
    replication_version INTEGER CHECK (replication_version IS NULL OR replication_version BETWEEN -2147483648 AND 2147483647),
    content_hash TEXT CHECK (content_hash IS NULL OR instr(content_hash,char(0)) = 0),
    revision_id TEXT REFERENCES config_revisions(id) ON DELETE NO ACTION ON UPDATE NO ACTION CHECK (revision_id IS NULL OR (
        instr(revision_id,char(0)) = 0 AND length(revision_id) = 36 AND substr(revision_id,9,1) = '-' AND substr(revision_id,14,1) = '-'
        AND substr(revision_id,19,1) = '-' AND substr(revision_id,24,1) = '-'
        AND length(replace(revision_id,'-','')) = 32 AND replace(revision_id,'-','') NOT GLOB '*[^0-9a-f]*')),
    apply_job_id TEXT REFERENCES apply_jobs(id) ON DELETE NO ACTION ON UPDATE NO ACTION CHECK (apply_job_id IS NULL OR (
        instr(apply_job_id,char(0)) = 0 AND length(apply_job_id) = 36 AND substr(apply_job_id,9,1) = '-' AND substr(apply_job_id,14,1) = '-'
        AND substr(apply_job_id,19,1) = '-' AND substr(apply_job_id,24,1) = '-'
        AND length(replace(apply_job_id,'-','')) = 32 AND replace(apply_job_id,'-','') NOT GLOB '*[^0-9a-f]*')),
    result_code TEXT CHECK (result_code IS NULL OR instr(result_code,char(0)) = 0),
    started_at TEXT COLLATE BINARY CHECK (started_at IS NULL OR (instr(started_at,char(0)) = 0 AND (
        started_at IN ('0','2') OR (length(started_at) = 31 AND substr(started_at,1,1) = '1'
        AND substr(started_at,22,1) = '.' AND substr(started_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(started_at,23,9) NOT GLOB '*[^0-9]*' AND substr(started_at,2,20) <= '18446744073709551615')))),
    finished_at TEXT COLLATE BINARY CHECK (finished_at IS NULL OR (instr(finished_at,char(0)) = 0 AND (
        finished_at IN ('0','2') OR (length(finished_at) = 31 AND substr(finished_at,1,1) = '1'
        AND substr(finished_at,22,1) = '.' AND substr(finished_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(finished_at,23,9) NOT GLOB '*[^0-9]*' AND substr(finished_at,2,20) <= '18446744073709551615')))),
    created_at TEXT COLLATE BINARY NOT NULL CHECK (instr(created_at,char(0)) = 0 AND (
        created_at IN ('0','2') OR (length(created_at) = 31 AND substr(created_at,1,1) = '1'
        AND substr(created_at,22,1) = '.' AND substr(created_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(created_at,23,9) NOT GLOB '*[^0-9]*' AND substr(created_at,2,20) <= '18446744073709551615'))),
    updated_at TEXT COLLATE BINARY NOT NULL CHECK (instr(updated_at,char(0)) = 0 AND (
        updated_at IN ('0','2') OR (length(updated_at) = 31 AND substr(updated_at,1,1) = '1'
        AND substr(updated_at,22,1) = '.' AND substr(updated_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(updated_at,23,9) NOT GLOB '*[^0-9]*' AND substr(updated_at,2,20) <= '18446744073709551615'))),
    CONSTRAINT sync_attempts_trigger_check CHECK ("trigger" IN ('enrollment','periodic','manual','restart'))
) STRICT;
CREATE INDEX sync_attempts_node_created_idx ON sync_attempts(node_id ASC,created_at ASC);
