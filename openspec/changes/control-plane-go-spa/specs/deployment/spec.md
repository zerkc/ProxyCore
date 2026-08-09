# Deployment Delta — Go API + Vite SPA

## MODIFIED Requirements

### DEPLOY-001 Compose boundaries

The Compose deployment MUST separate **api**, worker, control, PostgreSQL,
Nginx, and CoreDNS responsibilities; **api** and worker MUST NOT receive a raw
Docker socket. The Next.js `web` service MUST NOT be part of the default
Compose topology.

#### Scenario: api has no reload privilege

Given the Compose topology  
When service mounts and capabilities are inspected  
Then only the control boundary has the Docker socket  
And the `api` service has no Docker socket mount

#### Scenario: control plane is Go not Next

Given a default `docker compose up`  
When running services are listed  
Then `api` is present  
And `web` is not present

### DEPLOY-002 Reproducible checks

The deployment MUST provide health checks for `api`, migration instructions via
the tools-profile migrator (or successor), and honest reporting when Docker is
unavailable.

#### Scenario: api health

Given the `api` container is running  
When `GET /api/health` is requested on the published web port  
Then the response is HTTP 200 with JSON `ok: true`

#### Scenario: SPA is served by api

Given the `api` container includes a built UI dist  
When `GET /` is requested  
Then HTML for the SPA is returned (not a Next.js server)

## ADDED Requirements

### DEPLOY-003 Lightweight api image build

The `api` image build MUST compile the Go server and Vite SPA without invoking
`next build`.

#### Scenario: dockerfile has no next build

Given `infra/compose/Dockerfile.api`  
When inspected  
Then it does not run Next.js production build  
And it produces a runtime image containing the Go binary and static UI assets
