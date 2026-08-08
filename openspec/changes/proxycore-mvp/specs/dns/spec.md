# DNS and Forwarding Delta

## ADDED Requirements

### DNS-001 Typed managed zones

The system MUST validate internal zones and A, AAAA, CNAME, TXT, MX, and SRV
records, including TTL bounds, wildcard labels, zone containment, and CNAME
conflicts before saving or applying them.

#### Scenario: invalid record is rejected

Given a managed zone  
When a record has an unsupported type or invalid value  
Then the mutation is rejected and the active CoreDNS revision is unchanged

### DNS-002 Sole-resolver forwarding

The system MUST forward unmanaged names through the matching most-specific
suffix rule or the ordered default resolver pool, with sequential fallback.

#### Scenario: suffix rule wins

Given a default resolver pool and a normalized suffix rule  
When an unmanaged name matches the suffix  
Then the suffix pool is selected before the default pool

### DNS-003 Proxy answer mode

DNS-only records MUST answer with their configured values; proxied A, AAAA,
and CNAME records MUST answer with configured installation ingress addresses.
Only one eligible record per canonical hostname may be proxied.

#### Scenario: proxy toggle changes the answer

Given a valid A record with an origin and ingress address  
When proxy is enabled and applied  
Then CoreDNS answers with the ingress address rather than the origin

