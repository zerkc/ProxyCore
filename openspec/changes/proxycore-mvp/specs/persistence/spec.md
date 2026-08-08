# Persistence and Revision Delta

## ADDED Requirements

### PERSIST-001 Desired and applied state

The system MUST persist desired revisions separately from applied revisions and
MUST NOT mark a revision applied without validation and health evidence.

#### Scenario: invalid revision does not replace applied state

Given an applied good revision  
When a candidate fails validation  
Then the good revision remains active and the failure is recorded

### PERSIST-002 Secret boundaries

Secret values and certificate private keys MUST be encrypted at rest and MUST
NOT appear in ordinary API responses, audit records, logs, or rendered
non-secret configuration.

#### Scenario: audit redacts a secret change

Given a mutation containing a provider credential  
When the mutation is audited  
Then before/after values are redacted and only metadata is retained

### PERSIST-003 Retention safety

Retention cleanup MUST honor configured age or size limits while preserving
current desired state, current applied state, and active certificates.

#### Scenario: cleanup preserves live configuration

Given old operational artifacts and a current applied revision  
When cleanup runs  
Then old artifacts may be removed but the current revision remains available

