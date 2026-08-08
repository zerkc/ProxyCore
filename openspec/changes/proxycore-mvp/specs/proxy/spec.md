# Proxy and Stream Delta

## ADDED Requirements

### PROXY-001 Typed record-level proxy

The system MUST derive Nginx HTTP/HTTPS configuration only from proxied
A, AAAA, and CNAME records and MUST reject incomplete or conflicting settings.

#### Scenario: unsupported record cannot be proxied

Given a TXT, MX, or SRV record  
When proxy is enabled  
Then validation rejects the change before a candidate is rendered

### PROXY-002 Deterministic path policy

Path rules MUST be exact or literal prefixes; exact rules take precedence over
prefixes, then the longest prefix wins. Redirects MUST use only 301, 302, 307,
or 308, and arbitrary directives MUST be rejected.

#### Scenario: exact path wins

Given one matching prefix and one matching exact path  
When a request path matches both  
Then the exact path policy is selected

### PROXY-003 Stream safety

TCP and UDP stream listeners MUST target explicit literal upstreams and MUST
reject address/port/protocol conflicts.

#### Scenario: conflicting listener is rejected

Given an existing listener  
When another route claims the same address, port, and protocol  
Then the new route is rejected before apply

