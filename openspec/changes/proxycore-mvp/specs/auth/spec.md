# Authentication and Authorization Delta

## ADDED Requirements

### AUTH-001 Bootstrap

The system MUST create exactly one first active Owner through a one-time
bootstrap operation and MUST reject bootstrap after an Owner exists.

#### Scenario: first bootstrap succeeds

Given an installation with no users  
When a valid username and password are submitted  
Then one active Owner is created and the installation becomes administrable

#### Scenario: repeated bootstrap is rejected

Given an installation with an active Owner  
When bootstrap is submitted  
Then the request is rejected and no user is changed

### AUTH-002 Revocable local sessions

The system MUST hash passwords securely, issue HttpOnly sessions, and reject
expired or revoked sessions on every authenticated request.

#### Scenario: revoked session cannot mutate state

Given a valid session that has been revoked  
When the session calls a mutating endpoint  
Then the request is rejected and an authorization audit event is written

### AUTH-003 Role enforcement

The server MUST enforce Owner and Operator permissions for every mutation and
MUST protect the last active Owner from deletion or demotion.

#### Scenario: Operator cannot manage users

Given an authenticated Operator  
When the Operator changes a user  
Then the server rejects the request and records the denial

