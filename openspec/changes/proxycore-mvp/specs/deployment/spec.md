# Deployment Delta

## ADDED Requirements

### DEPLOY-001 Compose boundaries

The Compose deployment MUST separate web, worker, control, PostgreSQL, Nginx,
and CoreDNS responsibilities; web and worker MUST NOT receive a raw Docker
socket.

#### Scenario: web has no reload privilege

Given the Compose topology  
When service mounts and capabilities are inspected  
Then only the control boundary has the configured deployment-control mechanism

### DEPLOY-002 Reproducible checks

The deployment MUST provide health checks, migration/bootstrap instructions,
and commands for validating CoreDNS/Nginx candidates before production-like use.

#### Scenario: unavailable Docker is reported honestly

Given the Docker daemon is not running  
When the normal verification phase completes  
Then Docker-dependent checks are reported blocked or not run, never as passed

