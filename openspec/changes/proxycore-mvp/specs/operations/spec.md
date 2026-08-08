# Operations and Audit Delta

## ADDED Requirements

### OPS-001 Mutation audit

Every mutating action MUST record actor, resource, redacted before/after
values, timestamp, correlation id, and result.

#### Scenario: denied mutation is audited

Given an authenticated user without permission  
When the user attempts a mutation  
Then the operation is rejected and a redacted denial event is persisted

### OPS-002 Health visibility

The system MUST distinguish application, worker, CoreDNS, Nginx, upstream, and
forwarder health and MUST show last successful apply and certificate expiry risk.

#### Scenario: worker outage is visible

Given desired state exists and the worker is unavailable  
When health is requested  
Then desired state remains visible while worker/apply health is reported failed

