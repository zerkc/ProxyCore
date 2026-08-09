# Control-plane API Delta — Go ownership

## ADDED Requirements

### API-001 Route parity for SPA

The Go API MUST expose the JSON endpoints required by the operator SPA with
request/response shapes compatible with the previous Next handlers:

- `GET /api/health`, `GET /api/status`
- `GET|PUT /api/settings`
- `POST /api/apply`
- `GET|POST /api/users`, `PATCH|DELETE /api/users/{id}`
- `GET|POST /api/zones`, `GET|POST /api/zones/{id}/records`, `PATCH .../records/{id}`
- `GET|POST /api/streams`, `PATCH|DELETE /api/streams/{id}`
- `GET|POST /api/certificates`
- `GET /api/acme-challenge/{token}`

#### Scenario: unauthenticated status is rejected

Given no session  
When `GET /api/status` is called  
Then the response is HTTP 401

#### Scenario: health is public

Given no session  
When `GET /api/health` is called  
Then the response is HTTP 200

### API-002 Desired-state apply queue

Mutations that change DNS, proxy, streams, or settings MUST create revisions
and apply jobs consumable by the existing Node worker (same tables and
semantics).

#### Scenario: zone create queues apply

Given an authenticated operator  
When a zone is created successfully  
Then a desired revision and combined apply job exist for the worker
