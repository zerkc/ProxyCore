-- Canonical new installs follow the ordered migration registry, not ALTER history.
-- UUIDs and tagged clocks are explicit writes; JSON remains lossless TEXT.
CREATE TABLE installation_settings (
    id TEXT NOT NULL PRIMARY KEY CHECK (instr(id,char(0)) = 0),
    ingress_ipv4 TEXT CHECK (ingress_ipv4 IS NULL OR instr(ingress_ipv4,char(0)) = 0),
    ingress_ipv6 TEXT CHECK (ingress_ipv6 IS NULL OR instr(ingress_ipv6,char(0)) = 0),
    default_resolver_pool TEXT CHECK (default_resolver_pool IS NULL OR (instr(default_resolver_pool,char(0)) = 0 AND json_valid(default_resolver_pool))),
    forwarding_rules TEXT CHECK (forwarding_rules IS NULL OR (instr(forwarding_rules,char(0)) = 0 AND json_valid(forwarding_rules))),
    enrollment_hostnames TEXT CHECK (enrollment_hostnames IS NULL OR (instr(enrollment_hostnames,char(0)) = 0 AND json_valid(enrollment_hostnames))),
    retention_max_age_days INTEGER NOT NULL DEFAULT 7 CHECK (retention_max_age_days BETWEEN -2147483648 AND 2147483647),
    retention_max_size_mb INTEGER NOT NULL DEFAULT 50 CHECK (retention_max_size_mb BETWEEN -2147483648 AND 2147483647),
    current_desired_revision_id TEXT CHECK (current_desired_revision_id IS NULL OR instr(current_desired_revision_id,char(0)) = 0),
    current_applied_revision_id TEXT CHECK (current_applied_revision_id IS NULL OR instr(current_applied_revision_id,char(0)) = 0),
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

CREATE TABLE zones (
    id TEXT NOT NULL PRIMARY KEY CHECK (instr(id,char(0)) = 0) CHECK (
        length(id) = 36 AND substr(id,9,1) = '-' AND substr(id,14,1) = '-'
        AND substr(id,19,1) = '-' AND substr(id,24,1) = '-'
        AND length(replace(id,'-','')) = 32 AND replace(id,'-','') NOT GLOB '*[^0-9a-f]*'
    ),
    name TEXT NOT NULL CHECK (instr(name,char(0)) = 0),
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
CREATE UNIQUE INDEX zones_name_idx ON zones(name);

CREATE TABLE dns_records (
    id TEXT NOT NULL PRIMARY KEY CHECK (instr(id,char(0)) = 0) CHECK (
        length(id) = 36 AND substr(id,9,1) = '-' AND substr(id,14,1) = '-'
        AND substr(id,19,1) = '-' AND substr(id,24,1) = '-'
        AND length(replace(id,'-','')) = 32 AND replace(id,'-','') NOT GLOB '*[^0-9a-f]*'
    ),
    zone_id TEXT NOT NULL REFERENCES zones(id) ON DELETE CASCADE ON UPDATE NO ACTION CHECK (instr(zone_id,char(0)) = 0) CHECK (
        length(zone_id) = 36 AND substr(zone_id,9,1) = '-' AND substr(zone_id,14,1) = '-'
        AND substr(zone_id,19,1) = '-' AND substr(zone_id,24,1) = '-'
        AND length(replace(zone_id,'-','')) = 32 AND replace(zone_id,'-','') NOT GLOB '*[^0-9a-f]*'
    ),
    name TEXT NOT NULL CHECK (instr(name,char(0)) = 0),
    type TEXT NOT NULL CHECK (instr(type,char(0)) = 0) CHECK (type IN ('A','AAAA','CNAME','TXT','MX','SRV')),
    value TEXT NOT NULL CHECK (instr(value,char(0)) = 0 AND json_valid(value)),
    ttl INTEGER NOT NULL DEFAULT 300 CHECK (ttl BETWEEN -2147483648 AND 2147483647),
    enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
    proxied INTEGER NOT NULL DEFAULT 0 CHECK (proxied IN (0,1)),
    proxy_settings TEXT CHECK (proxy_settings IS NULL OR (instr(proxy_settings,char(0)) = 0 AND json_valid(proxy_settings))),
    comment TEXT CHECK (comment IS NULL OR instr(comment,char(0)) = 0),
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
CREATE UNIQUE INDEX dns_records_zone_name_id_idx ON dns_records(zone_id,name,id);

CREATE TABLE resolver_pools (
    id TEXT NOT NULL PRIMARY KEY CHECK (instr(id,char(0)) = 0) CHECK (
        length(id) = 36 AND substr(id,9,1) = '-' AND substr(id,14,1) = '-'
        AND substr(id,19,1) = '-' AND substr(id,24,1) = '-'
        AND length(replace(id,'-','')) = 32 AND replace(id,'-','') NOT GLOB '*[^0-9a-f]*'
    ),
    name TEXT NOT NULL CHECK (instr(name,char(0)) = 0),
    endpoints TEXT NOT NULL CHECK (instr(endpoints,char(0)) = 0 AND json_valid(endpoints)),
    is_default INTEGER NOT NULL DEFAULT 0 CHECK (is_default IN (0,1)),
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
CREATE UNIQUE INDEX resolver_pools_name_idx ON resolver_pools(name);

CREATE TABLE forwarding_rules (
    id TEXT NOT NULL PRIMARY KEY CHECK (instr(id,char(0)) = 0) CHECK (
        length(id) = 36 AND substr(id,9,1) = '-' AND substr(id,14,1) = '-'
        AND substr(id,19,1) = '-' AND substr(id,24,1) = '-'
        AND length(replace(id,'-','')) = 32 AND replace(id,'-','') NOT GLOB '*[^0-9a-f]*'
    ),
    suffix TEXT NOT NULL CHECK (instr(suffix,char(0)) = 0),
    pool_id TEXT NOT NULL REFERENCES resolver_pools(id) ON DELETE CASCADE ON UPDATE NO ACTION CHECK (instr(pool_id,char(0)) = 0) CHECK (
        length(pool_id) = 36 AND substr(pool_id,9,1) = '-' AND substr(pool_id,14,1) = '-'
        AND substr(pool_id,19,1) = '-' AND substr(pool_id,24,1) = '-'
        AND length(replace(pool_id,'-','')) = 32 AND replace(pool_id,'-','') NOT GLOB '*[^0-9a-f]*'
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
    )
) STRICT;
CREATE UNIQUE INDEX forwarding_rules_suffix_idx ON forwarding_rules(suffix);

CREATE TABLE stream_routes (
    id TEXT NOT NULL PRIMARY KEY CHECK (instr(id,char(0)) = 0) CHECK (
        length(id) = 36 AND substr(id,9,1) = '-' AND substr(id,14,1) = '-'
        AND substr(id,19,1) = '-' AND substr(id,24,1) = '-'
        AND length(replace(id,'-','')) = 32 AND replace(id,'-','') NOT GLOB '*[^0-9a-f]*'
    ),
    enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
    protocol TEXT NOT NULL CHECK (instr(protocol,char(0)) = 0) CHECK (protocol IN ('tcp','udp')),
    listen_address TEXT NOT NULL CHECK (instr(listen_address,char(0)) = 0),
    listen_port INTEGER NOT NULL CHECK (listen_port BETWEEN -2147483648 AND 2147483647),
    upstream TEXT NOT NULL CHECK (instr(upstream,char(0)) = 0 AND json_valid(upstream)),
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
CREATE UNIQUE INDEX stream_routes_listener_idx ON stream_routes(protocol,listen_address,listen_port);
