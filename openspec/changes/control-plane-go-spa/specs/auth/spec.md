# Auth Delta — Go API sessions

## MODIFIED Requirements

### AUTH-001 Bootstrap and local sessions

The Go API MUST implement one-time Owner bootstrap and local
username/password sessions compatible with existing PostgreSQL `users` and
`sessions` rows.

#### Scenario: first bootstrap creates owner

Given an installation with no active users  
When `POST /api/auth/bootstrap` is called with username and password (min 5)  
Then an Owner user is created  
And later bootstrap attempts are rejected

#### Scenario: login sets session cookie

Given a valid active user  
When `POST /api/auth/login` succeeds  
Then the response includes the public user  
And a `Set-Cookie` for the configured session cookie name is returned

#### Scenario: existing scrypt hashes verify

Given a password hash created by the previous Node crypto helper  
When the Go API verifies the same password  
Then authentication succeeds

### AUTH-002 Authorization on mutations

Authenticated routes MUST accept the session cookie or `Authorization: Bearer`
and MUST enforce Owner/Operator role checks equivalent to the Next handlers.
