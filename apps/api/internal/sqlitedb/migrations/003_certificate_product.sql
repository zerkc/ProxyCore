-- Effective migration-first column order; IDs and tagged clocks are explicit writes.
-- Ciphertext is opaque TEXT, not JSON or a decoded secret representation.
CREATE TABLE secrets (
    id TEXT NOT NULL PRIMARY KEY CHECK (instr(id,char(0)) = 0) CHECK (
        length(id) = 36 AND substr(id,9,1) = '-' AND substr(id,14,1) = '-'
        AND substr(id,19,1) = '-' AND substr(id,24,1) = '-'
        AND length(replace(id,'-','')) = 32 AND replace(id,'-','') NOT GLOB '*[^0-9a-f]*'
    ),
    purpose TEXT NOT NULL CHECK (instr(purpose,char(0)) = 0),
    ciphertext TEXT NOT NULL CHECK (instr(ciphertext,char(0)) = 0),
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

CREATE TABLE certificates (
    id TEXT NOT NULL PRIMARY KEY CHECK (instr(id,char(0)) = 0) CHECK (
        length(id) = 36 AND substr(id,9,1) = '-' AND substr(id,14,1) = '-'
        AND substr(id,19,1) = '-' AND substr(id,24,1) = '-'
        AND length(replace(id,'-','')) = 32 AND replace(id,'-','') NOT GLOB '*[^0-9a-f]*'
    ),
    hostnames TEXT NOT NULL CHECK (instr(hostnames,char(0)) = 0 AND json_valid(hostnames)),
    issuer TEXT NOT NULL CHECK (instr(issuer,char(0)) = 0) CHECK (issuer IN ('self-signed','uploaded','letsencrypt')),
    challenge TEXT NOT NULL CHECK (instr(challenge,char(0)) = 0) CHECK (challenge IN ('none','http-01','dns-01')),
    environment TEXT NOT NULL DEFAULT 'production' CHECK (instr(environment,char(0)) = 0),
    status TEXT NOT NULL DEFAULT 'pending' CHECK (instr(status,char(0)) = 0) CHECK (status IN ('pending','issued','active','expired','failed')),
    expires_at TEXT COLLATE BINARY CHECK (expires_at IS NULL OR (instr(expires_at,char(0)) = 0 AND (
        expires_at IN ('0','2') OR (length(expires_at) = 31 AND substr(expires_at,1,1) = '1'
        AND substr(expires_at,22,1) = '.' AND substr(expires_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(expires_at,23,9) NOT GLOB '*[^0-9]*' AND substr(expires_at,2,20) <= '18446744073709551615')
    ))),
    renew_after TEXT COLLATE BINARY CHECK (renew_after IS NULL OR (instr(renew_after,char(0)) = 0 AND (
        renew_after IN ('0','2') OR (length(renew_after) = 31 AND substr(renew_after,1,1) = '1'
        AND substr(renew_after,22,1) = '.' AND substr(renew_after,2,20) NOT GLOB '*[^0-9]*'
        AND substr(renew_after,23,9) NOT GLOB '*[^0-9]*' AND substr(renew_after,2,20) <= '18446744073709551615')
    ))),
    key_secret_id TEXT REFERENCES secrets(id) ON DELETE NO ACTION ON UPDATE NO ACTION CHECK (key_secret_id IS NULL OR (
        instr(key_secret_id,char(0)) = 0 AND length(key_secret_id) = 36 AND substr(key_secret_id,9,1) = '-' AND substr(key_secret_id,14,1) = '-'
        AND substr(key_secret_id,19,1) = '-' AND substr(key_secret_id,24,1) = '-'
        AND length(replace(key_secret_id,'-','')) = 32 AND replace(key_secret_id,'-','') NOT GLOB '*[^0-9a-f]*'
    )),
    certificate_pem TEXT CHECK (certificate_pem IS NULL OR instr(certificate_pem,char(0)) = 0),
    created_at TEXT COLLATE BINARY NOT NULL CHECK (instr(created_at,char(0)) = 0) CHECK (
        created_at IN ('0','2') OR (length(created_at) = 31 AND substr(created_at,1,1) = '1'
        AND substr(created_at,22,1) = '.' AND substr(created_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(created_at,23,9) NOT GLOB '*[^0-9]*' AND substr(created_at,2,20) <= '18446744073709551615')
    ),
    updated_at TEXT COLLATE BINARY NOT NULL CHECK (instr(updated_at,char(0)) = 0) CHECK (
        updated_at IN ('0','2') OR (length(updated_at) = 31 AND substr(updated_at,1,1) = '1'
        AND substr(updated_at,22,1) = '.' AND substr(updated_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(updated_at,23,9) NOT GLOB '*[^0-9]*' AND substr(updated_at,2,20) <= '18446744073709551615')
    ),
    failure_reason TEXT CHECK (failure_reason IS NULL OR instr(failure_reason,char(0)) = 0)
) STRICT;

CREATE TABLE provider_connections (
    id TEXT NOT NULL PRIMARY KEY CHECK (instr(id,char(0)) = 0) CHECK (
        length(id) = 36 AND substr(id,9,1) = '-' AND substr(id,14,1) = '-'
        AND substr(id,19,1) = '-' AND substr(id,24,1) = '-'
        AND length(replace(id,'-','')) = 32 AND replace(id,'-','') NOT GLOB '*[^0-9a-f]*'
    ),
    provider TEXT NOT NULL CHECK (instr(provider,char(0)) = 0),
    name TEXT NOT NULL CHECK (instr(name,char(0)) = 0),
    secret_id TEXT NOT NULL REFERENCES secrets(id) ON DELETE NO ACTION ON UPDATE NO ACTION CHECK (instr(secret_id,char(0)) = 0) CHECK (
        length(secret_id) = 36 AND substr(secret_id,9,1) = '-' AND substr(secret_id,14,1) = '-'
        AND substr(secret_id,19,1) = '-' AND substr(secret_id,24,1) = '-'
        AND length(replace(secret_id,'-','')) = 32 AND replace(secret_id,'-','') NOT GLOB '*[^0-9a-f]*'
    ),
    scope TEXT NOT NULL CHECK (instr(scope,char(0)) = 0),
    enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
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

CREATE TABLE internal_ca (
    id TEXT NOT NULL PRIMARY KEY CHECK (instr(id,char(0)) = 0),
    certificate_pem TEXT NOT NULL CHECK (instr(certificate_pem,char(0)) = 0),
    key_secret_id TEXT NOT NULL REFERENCES secrets(id) ON DELETE NO ACTION ON UPDATE NO ACTION CHECK (instr(key_secret_id,char(0)) = 0) CHECK (
        length(key_secret_id) = 36 AND substr(key_secret_id,9,1) = '-' AND substr(key_secret_id,14,1) = '-'
        AND substr(key_secret_id,19,1) = '-' AND substr(key_secret_id,24,1) = '-'
        AND length(replace(key_secret_id,'-','')) = 32 AND replace(key_secret_id,'-','') NOT GLOB '*[^0-9a-f]*'
    ),
    created_at TEXT COLLATE BINARY NOT NULL CHECK (instr(created_at,char(0)) = 0) CHECK (
        created_at IN ('0','2') OR (length(created_at) = 31 AND substr(created_at,1,1) = '1'
        AND substr(created_at,22,1) = '.' AND substr(created_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(created_at,23,9) NOT GLOB '*[^0-9]*' AND substr(created_at,2,20) <= '18446744073709551615')
    ),
    updated_at TEXT COLLATE BINARY NOT NULL CHECK (instr(updated_at,char(0)) = 0) CHECK (
        updated_at IN ('0','2') OR (length(updated_at) = 31 AND substr(updated_at,1,1) = '1'
        AND substr(updated_at,22,1) = '.' AND substr(updated_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(updated_at,23,9) NOT GLOB '*[^0-9]*' AND substr(updated_at,2,20) <= '18446744073709551615')
    ),
    enrollment_certificate_pem TEXT CHECK (enrollment_certificate_pem IS NULL OR instr(enrollment_certificate_pem,char(0)) = 0),
    enrollment_key_secret_id TEXT REFERENCES secrets(id) ON DELETE NO ACTION ON UPDATE NO ACTION CHECK (enrollment_key_secret_id IS NULL OR (
        instr(enrollment_key_secret_id,char(0)) = 0 AND length(enrollment_key_secret_id) = 36 AND substr(enrollment_key_secret_id,9,1) = '-' AND substr(enrollment_key_secret_id,14,1) = '-'
        AND substr(enrollment_key_secret_id,19,1) = '-' AND substr(enrollment_key_secret_id,24,1) = '-'
        AND length(replace(enrollment_key_secret_id,'-','')) = 32 AND replace(enrollment_key_secret_id,'-','') NOT GLOB '*[^0-9a-f]*'
    ))
) STRICT;

CREATE TABLE internal_ca_enrollment_state (
    id TEXT NOT NULL PRIMARY KEY CHECK (instr(id,char(0)) = 0),
    established_at TEXT COLLATE BINARY NOT NULL CHECK (instr(established_at,char(0)) = 0) CHECK (
        established_at IN ('0','2') OR (length(established_at) = 31 AND substr(established_at,1,1) = '1'
        AND substr(established_at,22,1) = '.' AND substr(established_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(established_at,23,9) NOT GLOB '*[^0-9]*' AND substr(established_at,2,20) <= '18446744073709551615')
    )
) STRICT;
