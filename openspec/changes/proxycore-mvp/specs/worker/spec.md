# Worker and Apply Delta

## ADDED Requirements

### WORKER-001 Fixed control protocol

The worker MUST communicate with the service-control helper through a private
Unix socket using a fixed operation allowlist and MUST NOT send arbitrary
commands or directives.

#### Scenario: unknown helper operation is rejected

Given a connected worker client  
When it requests an unknown operation or includes a shell command  
Then the helper rejects the request and records the protocol failure

### WORKER-002 Validate before promotion

The worker MUST render from one consistent snapshot, validate candidates, then
promote/reload and health-check them; failed validation or health MUST preserve
the last known-good revision or execute documented rollback.

#### Scenario: health failure rolls back

Given a valid candidate and an active revision  
When reload succeeds but health fails  
Then the worker rolls back or marks explicit recovery required without false
applied success

### WORKER-003 Job serialization

Concurrent jobs targeting the same service MUST be serialized or safely
coalesced and each job MUST expose revision, actor, timestamps, output, and
final status.

#### Scenario: same-service jobs serialize

Given two queued CoreDNS jobs  
When the worker claims them concurrently  
Then only one is applying at a time and both have observable final outcomes

