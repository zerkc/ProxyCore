-- Migration-first order: continuity attributes follow the original timestamps.
-- UUIDs and tagged clocks are supplied by writers; source attribution is optional.
CREATE TABLE config_revisions (
    id TEXT NOT NULL PRIMARY KEY CHECK (instr(id,char(0)) = 0 AND
        length(id) = 36 AND substr(id,9,1) = '-' AND substr(id,14,1) = '-'
        AND substr(id,19,1) = '-' AND substr(id,24,1) = '-'
        AND length(replace(id,'-','')) = 32 AND replace(id,'-','') NOT GLOB '*[^0-9a-f]*'),
    revision_number INTEGER NOT NULL CHECK (revision_number BETWEEN -2147483648 AND 2147483647),
    checksum TEXT NOT NULL CHECK (instr(checksum,char(0)) = 0),
    snapshot TEXT NOT NULL CHECK (instr(snapshot,char(0)) = 0 AND json_valid(snapshot)),
    actor_user_id TEXT REFERENCES users(id) ON DELETE NO ACTION ON UPDATE NO ACTION CHECK (actor_user_id IS NULL OR (
        instr(actor_user_id,char(0)) = 0 AND length(actor_user_id) = 36 AND substr(actor_user_id,9,1) = '-' AND substr(actor_user_id,14,1) = '-'
        AND substr(actor_user_id,19,1) = '-' AND substr(actor_user_id,24,1) = '-'
        AND length(replace(actor_user_id,'-','')) = 32 AND replace(actor_user_id,'-','') NOT GLOB '*[^0-9a-f]*')),
    created_at TEXT COLLATE BINARY NOT NULL CHECK (instr(created_at,char(0)) = 0 AND (
        created_at IN ('0','2') OR (length(created_at) = 31 AND substr(created_at,1,1) = '1'
        AND substr(created_at,22,1) = '.' AND substr(created_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(created_at,23,9) NOT GLOB '*[^0-9]*' AND substr(created_at,2,20) <= '18446744073709551615'))),
    applied_at TEXT COLLATE BINARY CHECK (applied_at IS NULL OR (instr(applied_at,char(0)) = 0 AND (
        applied_at IN ('0','2') OR (length(applied_at) = 31 AND substr(applied_at,1,1) = '1'
        AND substr(applied_at,22,1) = '.' AND substr(applied_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(applied_at,23,9) NOT GLOB '*[^0-9]*' AND substr(applied_at,2,20) <= '18446744073709551615')))),
    source TEXT NOT NULL DEFAULT 'ordinary' CHECK (instr(source,char(0)) = 0),
    source_primary_id TEXT CHECK (source_primary_id IS NULL OR (
        instr(source_primary_id,char(0)) = 0 AND length(source_primary_id) = 36 AND substr(source_primary_id,9,1) = '-' AND substr(source_primary_id,14,1) = '-'
        AND substr(source_primary_id,19,1) = '-' AND substr(source_primary_id,24,1) = '-'
        AND length(replace(source_primary_id,'-','')) = 32 AND replace(source_primary_id,'-','') NOT GLOB '*[^0-9a-f]*')),
    source_node_id TEXT CHECK (source_node_id IS NULL OR (
        instr(source_node_id,char(0)) = 0 AND length(source_node_id) = 36 AND substr(source_node_id,9,1) = '-' AND substr(source_node_id,14,1) = '-'
        AND substr(source_node_id,19,1) = '-' AND substr(source_node_id,24,1) = '-'
        AND length(replace(source_node_id,'-','')) = 32 AND replace(source_node_id,'-','') NOT GLOB '*[^0-9a-f]*')),
    source_revision_id TEXT CHECK (source_revision_id IS NULL OR (
        instr(source_revision_id,char(0)) = 0 AND length(source_revision_id) = 36 AND substr(source_revision_id,9,1) = '-' AND substr(source_revision_id,14,1) = '-'
        AND substr(source_revision_id,19,1) = '-' AND substr(source_revision_id,24,1) = '-'
        AND length(replace(source_revision_id,'-','')) = 32 AND replace(source_revision_id,'-','') NOT GLOB '*[^0-9a-f]*')),
    snapshot_content_hash TEXT CHECK (snapshot_content_hash IS NULL OR instr(snapshot_content_hash,char(0)) = 0),
    snapshot_version INTEGER CHECK (snapshot_version IS NULL OR snapshot_version BETWEEN -2147483648 AND 2147483647),
    replication_version INTEGER CHECK (replication_version IS NULL OR replication_version BETWEEN -2147483648 AND 2147483647),
    leadership_generation INTEGER,
    CONSTRAINT config_revisions_source_check CHECK (source IN ('ordinary','import','sync'))
) STRICT;
CREATE UNIQUE INDEX config_revisions_number_idx ON config_revisions(revision_number);
CREATE UNIQUE INDEX config_revisions_checksum_idx ON config_revisions(checksum);

CREATE TABLE apply_jobs (
    id TEXT NOT NULL PRIMARY KEY CHECK (instr(id,char(0)) = 0 AND
        length(id) = 36 AND substr(id,9,1) = '-' AND substr(id,14,1) = '-'
        AND substr(id,19,1) = '-' AND substr(id,24,1) = '-'
        AND length(replace(id,'-','')) = 32 AND replace(id,'-','') NOT GLOB '*[^0-9a-f]*'),
    revision_id TEXT NOT NULL REFERENCES config_revisions(id) ON DELETE NO ACTION ON UPDATE NO ACTION CHECK (
        instr(revision_id,char(0)) = 0 AND length(revision_id) = 36 AND substr(revision_id,9,1) = '-' AND substr(revision_id,14,1) = '-'
        AND substr(revision_id,19,1) = '-' AND substr(revision_id,24,1) = '-'
        AND length(replace(revision_id,'-','')) = 32 AND replace(revision_id,'-','') NOT GLOB '*[^0-9a-f]*'),
    actor_user_id TEXT REFERENCES users(id) ON DELETE NO ACTION ON UPDATE NO ACTION CHECK (actor_user_id IS NULL OR (
        instr(actor_user_id,char(0)) = 0 AND length(actor_user_id) = 36 AND substr(actor_user_id,9,1) = '-' AND substr(actor_user_id,14,1) = '-'
        AND substr(actor_user_id,19,1) = '-' AND substr(actor_user_id,24,1) = '-'
        AND length(replace(actor_user_id,'-','')) = 32 AND replace(actor_user_id,'-','') NOT GLOB '*[^0-9a-f]*')),
    target TEXT NOT NULL CHECK (instr(target,char(0)) = 0 AND target IN ('coredns','nginx','combined','certificate')),
    status TEXT NOT NULL DEFAULT 'queued' CHECK (instr(status,char(0)) = 0 AND status IN ('queued','validating','applying','applied','failed','rolled-back')),
    correlation_id TEXT NOT NULL CHECK (instr(correlation_id,char(0)) = 0),
    validation_output TEXT CHECK (validation_output IS NULL OR (instr(validation_output,char(0)) = 0 AND json_valid(validation_output))),
    apply_output TEXT CHECK (apply_output IS NULL OR (instr(apply_output,char(0)) = 0 AND json_valid(apply_output))),
    health_output TEXT CHECK (health_output IS NULL OR (instr(health_output,char(0)) = 0 AND json_valid(health_output))),
    error_message TEXT CHECK (error_message IS NULL OR instr(error_message,char(0)) = 0),
    claimed_at TEXT COLLATE BINARY CHECK (claimed_at IS NULL OR (instr(claimed_at,char(0)) = 0 AND (
        claimed_at IN ('0','2') OR (length(claimed_at) = 31 AND substr(claimed_at,1,1) = '1'
        AND substr(claimed_at,22,1) = '.' AND substr(claimed_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(claimed_at,23,9) NOT GLOB '*[^0-9]*' AND substr(claimed_at,2,20) <= '18446744073709551615')))),
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
    source TEXT NOT NULL DEFAULT 'ordinary' CHECK (instr(source,char(0)) = 0),
    source_primary_id TEXT CHECK (source_primary_id IS NULL OR (
        instr(source_primary_id,char(0)) = 0 AND length(source_primary_id) = 36 AND substr(source_primary_id,9,1) = '-' AND substr(source_primary_id,14,1) = '-'
        AND substr(source_primary_id,19,1) = '-' AND substr(source_primary_id,24,1) = '-'
        AND length(replace(source_primary_id,'-','')) = 32 AND replace(source_primary_id,'-','') NOT GLOB '*[^0-9a-f]*')),
    source_node_id TEXT CHECK (source_node_id IS NULL OR (
        instr(source_node_id,char(0)) = 0 AND length(source_node_id) = 36 AND substr(source_node_id,9,1) = '-' AND substr(source_node_id,14,1) = '-'
        AND substr(source_node_id,19,1) = '-' AND substr(source_node_id,24,1) = '-'
        AND length(replace(source_node_id,'-','')) = 32 AND replace(source_node_id,'-','') NOT GLOB '*[^0-9a-f]*')),
    source_revision_id TEXT CHECK (source_revision_id IS NULL OR (
        instr(source_revision_id,char(0)) = 0 AND length(source_revision_id) = 36 AND substr(source_revision_id,9,1) = '-' AND substr(source_revision_id,14,1) = '-'
        AND substr(source_revision_id,19,1) = '-' AND substr(source_revision_id,24,1) = '-'
        AND length(replace(source_revision_id,'-','')) = 32 AND replace(source_revision_id,'-','') NOT GLOB '*[^0-9a-f]*')),
    snapshot_content_hash TEXT CHECK (snapshot_content_hash IS NULL OR instr(snapshot_content_hash,char(0)) = 0),
    snapshot_version INTEGER CHECK (snapshot_version IS NULL OR snapshot_version BETWEEN -2147483648 AND 2147483647),
    replication_version INTEGER CHECK (replication_version IS NULL OR replication_version BETWEEN -2147483648 AND 2147483647),
    leadership_generation INTEGER,
    CONSTRAINT apply_jobs_source_check CHECK (source IN ('ordinary','import','sync'))
) STRICT;

CREATE TABLE health_observations (
    id TEXT NOT NULL PRIMARY KEY CHECK (instr(id,char(0)) = 0 AND
        length(id) = 36 AND substr(id,9,1) = '-' AND substr(id,14,1) = '-'
        AND substr(id,19,1) = '-' AND substr(id,24,1) = '-'
        AND length(replace(id,'-','')) = 32 AND replace(id,'-','') NOT GLOB '*[^0-9a-f]*'),
    component TEXT NOT NULL CHECK (instr(component,char(0)) = 0),
    status TEXT NOT NULL CHECK (instr(status,char(0)) = 0),
    details TEXT CHECK (details IS NULL OR (instr(details,char(0)) = 0 AND json_valid(details))),
    created_at TEXT COLLATE BINARY NOT NULL CHECK (instr(created_at,char(0)) = 0 AND (
        created_at IN ('0','2') OR (length(created_at) = 31 AND substr(created_at,1,1) = '1'
        AND substr(created_at,22,1) = '.' AND substr(created_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(created_at,23,9) NOT GLOB '*[^0-9]*' AND substr(created_at,2,20) <= '18446744073709551615')))
) STRICT;

CREATE TABLE operational_artifacts (
    id TEXT NOT NULL PRIMARY KEY CHECK (instr(id,char(0)) = 0 AND
        length(id) = 36 AND substr(id,9,1) = '-' AND substr(id,14,1) = '-'
        AND substr(id,19,1) = '-' AND substr(id,24,1) = '-'
        AND length(replace(id,'-','')) = 32 AND replace(id,'-','') NOT GLOB '*[^0-9a-f]*'),
    kind TEXT NOT NULL CHECK (instr(kind,char(0)) = 0),
    path TEXT NOT NULL CHECK (instr(path,char(0)) = 0),
    size_bytes INTEGER NOT NULL CHECK (size_bytes BETWEEN -2147483648 AND 2147483647),
    revision_id TEXT REFERENCES config_revisions(id) ON DELETE NO ACTION ON UPDATE NO ACTION CHECK (revision_id IS NULL OR (
        instr(revision_id,char(0)) = 0 AND length(revision_id) = 36 AND substr(revision_id,9,1) = '-' AND substr(revision_id,14,1) = '-'
        AND substr(revision_id,19,1) = '-' AND substr(revision_id,24,1) = '-'
        AND length(replace(revision_id,'-','')) = 32 AND replace(revision_id,'-','') NOT GLOB '*[^0-9a-f]*')),
    created_at TEXT COLLATE BINARY NOT NULL CHECK (instr(created_at,char(0)) = 0 AND (
        created_at IN ('0','2') OR (length(created_at) = 31 AND substr(created_at,1,1) = '1'
        AND substr(created_at,22,1) = '.' AND substr(created_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(created_at,23,9) NOT GLOB '*[^0-9]*' AND substr(created_at,2,20) <= '18446744073709551615')))
) STRICT;
