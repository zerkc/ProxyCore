-- IDs and tagged clocks are supplied explicitly by the shared write wrapper.
-- PostgreSQL UUID/time defaults are intentionally absent; boolean defaults remain.
-- STRICT rejects BLOB text storage; SQLite lossless affinity conversions still apply.
CREATE TABLE users (
    id TEXT NOT NULL PRIMARY KEY CHECK (instr(id,char(0)) = 0) CHECK (
        length(id) = 36 AND substr(id,9,1) = '-' AND substr(id,14,1) = '-'
        AND substr(id,19,1) = '-' AND substr(id,24,1) = '-'
        AND length(replace(id,'-','')) = 32 AND replace(id,'-','') NOT GLOB '*[^0-9a-f]*'
    ),
    username TEXT NOT NULL CHECK (instr(username,char(0)) = 0),
    password_hash TEXT NOT NULL CHECK (instr(password_hash,char(0)) = 0),
    role TEXT NOT NULL CHECK (role IN ('owner','operator')),
    active INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0,1)),
    password_change_required INTEGER NOT NULL DEFAULT 0 CHECK (password_change_required IN (0,1)),
    created_at TEXT COLLATE BINARY NOT NULL CHECK (instr(created_at,char(0)) = 0) CHECK (
        created_at IN ('0','2') OR (length(created_at) = 31 AND substr(created_at,1,1) = '1'
        AND substr(created_at,22,1) = '.' AND substr(created_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(created_at,23,9) NOT GLOB '*[^0-9]*' AND substr(created_at,2,20) <= '18446744073709551615')
    ),
    updated_at TEXT COLLATE BINARY NOT NULL CHECK (instr(updated_at,char(0)) = 0) CHECK (
        updated_at IN ('0','2') OR (length(updated_at) = 31 AND substr(updated_at,1,1) = '1'
        AND substr(updated_at,22,1) = '.' AND substr(updated_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(updated_at,23,9) NOT GLOB '*[^0-9]*' AND substr(updated_at,2,20) <= '18446744073709551615')
    )
) STRICT;
CREATE UNIQUE INDEX users_username_idx ON users(username);

CREATE TABLE sessions (
    id TEXT NOT NULL PRIMARY KEY CHECK (instr(id,char(0)) = 0) CHECK (
        length(id) = 36 AND substr(id,9,1) = '-' AND substr(id,14,1) = '-'
        AND substr(id,19,1) = '-' AND substr(id,24,1) = '-'
        AND length(replace(id,'-','')) = 32 AND replace(id,'-','') NOT GLOB '*[^0-9a-f]*'
    ),
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE CHECK (instr(user_id,char(0)) = 0),
    token_hash TEXT NOT NULL CHECK (instr(token_hash,char(0)) = 0),
    expires_at TEXT COLLATE BINARY NOT NULL CHECK (instr(expires_at,char(0)) = 0) CHECK (
        expires_at IN ('0','2') OR (length(expires_at) = 31 AND substr(expires_at,1,1) = '1'
        AND substr(expires_at,22,1) = '.' AND substr(expires_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(expires_at,23,9) NOT GLOB '*[^0-9]*' AND substr(expires_at,2,20) <= '18446744073709551615')
    ),
    revoked_at TEXT COLLATE BINARY CHECK (revoked_at IS NULL OR instr(revoked_at,char(0)) = 0) CHECK (
        revoked_at IS NULL OR revoked_at IN ('0','2') OR (length(revoked_at) = 31 AND substr(revoked_at,1,1) = '1'
        AND substr(revoked_at,22,1) = '.' AND substr(revoked_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(revoked_at,23,9) NOT GLOB '*[^0-9]*' AND substr(revoked_at,2,20) <= '18446744073709551615')
    ),
    last_seen_at TEXT COLLATE BINARY CHECK (last_seen_at IS NULL OR instr(last_seen_at,char(0)) = 0) CHECK (
        last_seen_at IS NULL OR last_seen_at IN ('0','2') OR (length(last_seen_at) = 31 AND substr(last_seen_at,1,1) = '1'
        AND substr(last_seen_at,22,1) = '.' AND substr(last_seen_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(last_seen_at,23,9) NOT GLOB '*[^0-9]*' AND substr(last_seen_at,2,20) <= '18446744073709551615')
    ),
    created_at TEXT COLLATE BINARY NOT NULL CHECK (instr(created_at,char(0)) = 0) CHECK (
        created_at IN ('0','2') OR (length(created_at) = 31 AND substr(created_at,1,1) = '1'
        AND substr(created_at,22,1) = '.' AND substr(created_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(created_at,23,9) NOT GLOB '*[^0-9]*' AND substr(created_at,2,20) <= '18446744073709551615')
    )
) STRICT;
CREATE UNIQUE INDEX sessions_token_hash_idx ON sessions(token_hash);

CREATE TABLE audit_events (
    id TEXT NOT NULL PRIMARY KEY CHECK (instr(id,char(0)) = 0) CHECK (
        length(id) = 36 AND substr(id,9,1) = '-' AND substr(id,14,1) = '-'
        AND substr(id,19,1) = '-' AND substr(id,24,1) = '-'
        AND length(replace(id,'-','')) = 32 AND replace(id,'-','') NOT GLOB '*[^0-9a-f]*'
    ),
    actor_user_id TEXT REFERENCES users(id) ON DELETE NO ACTION CHECK (actor_user_id IS NULL OR instr(actor_user_id,char(0)) = 0),
    action TEXT NOT NULL CHECK (instr(action,char(0)) = 0),
    resource_type TEXT NOT NULL CHECK (instr(resource_type,char(0)) = 0),
    resource_id TEXT CHECK (resource_id IS NULL OR instr(resource_id,char(0)) = 0),
    before_value TEXT CHECK (before_value IS NULL OR (instr(before_value,char(0)) = 0 AND json_valid(before_value))),
    after_value TEXT CHECK (after_value IS NULL OR (instr(after_value,char(0)) = 0 AND json_valid(after_value))),
    correlation_id TEXT NOT NULL CHECK (instr(correlation_id,char(0)) = 0),
    result TEXT NOT NULL CHECK (instr(result,char(0)) = 0),
    created_at TEXT COLLATE BINARY NOT NULL CHECK (instr(created_at,char(0)) = 0) CHECK (
        created_at IN ('0','2') OR (length(created_at) = 31 AND substr(created_at,1,1) = '1'
        AND substr(created_at,22,1) = '.' AND substr(created_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(created_at,23,9) NOT GLOB '*[^0-9]*' AND substr(created_at,2,20) <= '18446744073709551615')
    )
) STRICT;
