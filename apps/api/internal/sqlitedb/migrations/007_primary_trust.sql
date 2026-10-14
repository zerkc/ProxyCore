-- Per-origin trust anchors a NODE uses to verify a PRIMARY that serves a
-- self-signed or private-CA certificate. One CA per primary origin; loading a
-- new CA for the same origin replaces the previous one.
CREATE TABLE primary_trust_anchors (
    origin TEXT NOT NULL PRIMARY KEY CHECK (instr(origin,char(0)) = 0 AND length(origin) BETWEEN 9 AND 300
        AND substr(origin,1,8) = 'https://'),
    ca_pem TEXT NOT NULL CHECK (instr(ca_pem,char(0)) = 0 AND length(ca_pem) BETWEEN 1 AND 65536),
    ca_der_sha256 TEXT NOT NULL CHECK (instr(ca_der_sha256,char(0)) = 0 AND length(ca_der_sha256) = 64
        AND ca_der_sha256 NOT GLOB '*[^0-9a-f]*'),
    updated_at TEXT COLLATE BINARY NOT NULL CHECK (instr(updated_at,char(0)) = 0 AND (
        updated_at IN ('0','2') OR (length(updated_at) = 31 AND substr(updated_at,1,1) = '1'
        AND substr(updated_at,22,1) = '.' AND substr(updated_at,2,20) NOT GLOB '*[^0-9]*'
        AND substr(updated_at,23,9) NOT GLOB '*[^0-9]*' AND substr(updated_at,2,20) <= '18446744073709551615')))
) STRICT;
