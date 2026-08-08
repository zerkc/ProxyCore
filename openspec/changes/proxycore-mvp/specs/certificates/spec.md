# Certificates and Provider Delta

## ADDED Requirements

### CERT-001 Certificate lifecycle

The system MUST support self-signed certificates and adapter-backed ACME
HTTP-01 and Cloudflare DNS-01 issuance, renewal, status, expiry, and staging.

#### Scenario: self-signed issuance

Given a valid proxied hostname with TLS enabled  
When a self-signed certificate job succeeds  
Then the certificate metadata is stored and the private key is encrypted

### CERT-002 Safe renewal

The system MUST keep the active verified certificate when a renewal candidate
fails validation, provider communication, or installation.

#### Scenario: failed renewal preserves active certificate

Given an active valid certificate  
When renewal fails  
Then the active certificate remains selected and expiry risk is visible

### CERT-003 Cloudflare scope

The Cloudflare adapter MUST create, observe, and remove only the
`_acme-challenge` TXT records owned by the certificate job.

#### Scenario: unrelated record cannot be changed

Given a Cloudflare provider connection  
When a request targets an ordinary DNS record  
Then the adapter rejects it without making a provider mutation

