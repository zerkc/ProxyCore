-- A PRIMARY records when each enrolled node last made an authenticated pull or
-- acknowledgement and the gateway (ingress) address the node reported. The IP
-- columns are validated and length-bounded by the application.
ALTER TABLE enrolled_nodes ADD COLUMN last_seen_at TEXT COLLATE BINARY CHECK (last_seen_at IS NULL OR (instr(last_seen_at,char(0)) = 0 AND (
    last_seen_at IN ('0','2') OR (length(last_seen_at) = 31 AND substr(last_seen_at,1,1) = '1'
    AND substr(last_seen_at,22,1) = '.' AND substr(last_seen_at,2,20) NOT GLOB '*[^0-9]*'
    AND substr(last_seen_at,23,9) NOT GLOB '*[^0-9]*' AND substr(last_seen_at,2,20) <= '18446744073709551615'))));
ALTER TABLE enrolled_nodes ADD COLUMN ingress_ipv4 TEXT CHECK (ingress_ipv4 IS NULL OR instr(ingress_ipv4,char(0)) = 0);
ALTER TABLE enrolled_nodes ADD COLUMN ingress_ipv6 TEXT CHECK (ingress_ipv6 IS NULL OR instr(ingress_ipv6,char(0)) = 0);
