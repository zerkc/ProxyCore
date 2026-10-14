# ProxyCore

[![CI](https://github.com/zerkc/ProxyCore/actions/workflows/ci.yml/badge.svg)](https://github.com/zerkc/ProxyCore/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

Single-host homelab control plane for **local DNS** and **secure ingress**.

**Version:** `0.4.0`

Manage zones and records in a Cloudflare-style UI, toggle proxy on A/AAAA/CNAME, issue certificates, and forward TCP/UDP streams — without hand-editing CoreDNS or Nginx.

## What it does

| Job | Behavior |
| --- | --- |
| Sole homelab DNS | CoreDNS is authoritative for managed zones and forwards everything else to your resolvers (Pi-hole, AdGuard, public DNS, …). |
| Cloudflare-style proxy | Proxied records resolve to this host’s ingress address; Nginx terminates TLS and forwards to an explicit origin IP:port. |
| Certificates | Self-signed, Let’s Encrypt HTTP-01, or Let’s Encrypt DNS-01 via Cloudflare (`_acme-challenge` only). |
| Streams | Separate TCP/UDP listeners → upstream IP:port (Nginx stream). |
| Safe apply | Desired state in SQLite → worker renders → validates → reloads → rolls back on failure. |
| Local operators | One-time Owner bootstrap; Owner/Operator roles; no OIDC/MFA in the MVP. |

Control plane edge is **Go API + Vite SPA**. Next.js is removed from the dependency tree, and the Go API now serves every configuration route (auth, settings, zones, records, streams, certificates, and the ACME HTTP-01 challenge) directly — the transitional Node `node-api` has been retired. The worker never gets a raw Docker socket; a small helper applies fixed Nginx/CoreDNS operations over a private Unix socket.

## Quick install / update

Linux host with Docker Engine (Compose v2) and Git:

```sh
curl -fsSL https://raw.githubusercontent.com/zerkc/ProxyCore/main/scripts/install.sh | sh
```

That clones or updates the checkout (`/opt/proxycore` as root, otherwise `~/proxycore`), creates `.env` with generated secrets on first run, applies migrations, and starts the stack. **Nginx runs with `network_mode: host`** so HTTP/HTTPS and stream ports bind on the host. The API also publishes `ENROLLMENT_PORT` (default `3443`) for direct TLS enrollment, independent of `WEB_PORT`; it is not Nginx proxy ingress. This deployment unit reserves the port but does not start the enrollment listener yet.

Then open `http://<host-ip>:3000/bootstrap` once to create the Owner.

| Variable | Default | Purpose |
| --- | --- | --- |
| `PROXYCORE_HOME` | `/opt/proxycore` or `~/proxycore` | Install directory |
| `PROXYCORE_BRANCH` | `main` | Git branch |
| `WEB_PORT` | `3000` | Dashboard / API |
| `ENROLLMENT_PORT` | `3443` | Direct TLS enrollment, separate from Nginx proxy ingress |
| `DNS_PORT` | `53` | CoreDNS |
| `SKIP_BUILD` | `0` | Set `1` to recreate without rebuild |
| `PROXYCORE_UPDATE_CHECK_ENABLED` | `1` | Set `0` to disable stable release checks against GitHub |
| `PROXYCORE_UPDATE_CHECK_INTERVAL` | `6h` | Minimum time between GitHub release checks |
| `PROXYCORE_UPDATE_CHECK_TIMEOUT` | `5s` | Timeout for each GitHub release request |

### Low-memory hosts (Raspberry Pi 3 B+ / 1 GB)

The installer builds the images one at a time (`[n/N] building <service>` with the
elapsed seconds after each one), then starts the stack. Peak memory stays at one
image build, but the first build can still take a long time on a small host.

- Add at least 1 GB of swap before installing. The installer only warns when RAM
  is below about 1.5 GB and swap below 1 GB; it never creates swap or uses sudo.
  Example: `sudo fallocate -l 1G /swapfile && sudo chmod 600 /swapfile && sudo mkswap /swapfile && sudo swapon /swapfile`
  (add the file to `/etc/fstab` to keep it after a reboot).
- Prefer a USB SSD over an SD card for the Docker data directory and the swap file.
- Set `PROXYCORE_SKIP_MEMORY_CHECK=1` to silence the warning.
- `SKIP_BUILD=1` recreates the services without building anything.
- Prebuilt multi-arch images are not published yet, so every install builds locally.
- A failed build stops the installer and names the service; retry it with
  `docker compose build <service>` and run the installer again.

Details: [docs/runbooks/bootstrap.md](docs/runbooks/bootstrap.md).

The dashboard checks the latest stable GitHub release on demand and shows a
link when a newer version is available. The result is cached by the API; this
feature only informs you and never updates the installation automatically.

## Converting between PRIMARY and NODE

Owners can change an installation's role from the dashboard (Continuity):

- **Convert to NODE** (on a PRIMARY): revokes the credentials of the nodes
  enrolled to this primary, makes the installation a read-only NODE, and
  continues into the enrollment form (enrollment token + PRIMARY URL, preview,
  confirm).
- **Promote to PRIMARY** (on a NODE): bumps the generation, retires the cluster
  key (a new one is minted lazily on the next enrollment or publication) and
  revokes the previous node credentials. Nodes of the former primary must be
  re-enrolled manually against the new primary. If the former primary is still
  running, it becomes a stale primary and must rejoin explicitly as a NODE.

Both actions need an explicit confirmation and are available through
`POST /api/topology/promote` and `POST /api/topology/demote` (owner only,
optional `expectedGeneration`).

## Stack

Docker Compose on one Linux host:

- **api** — Go control plane (SPA + auth + all configuration routes + ACME challenge)  
- **sqlite-data** volume — one SQLite file (desired state, users, jobs, audit) shared by api and worker  
- **worker** — render, validate, apply, health checks (Node)  
- **control** — fixed service-control helper (Docker socket only here)  
- **coredns** — authoritative zones + forwarders  
- **nginx** — HTTP/HTTPS proxy and TCP/UDP streams (host network)

## Not in scope (MVP)

Public authoritative DNS, Cloudflare Tunnel lifecycle, ordinary Cloudflare record sync, host firewall/NAT/fail2ban, Docker service discovery, OIDC/MFA, multi-tenant isolation, HA, or backup-based host-loss recovery.

## Docs

| Doc | Contents |
| --- | --- |
| [Product definition](docs/product-definition.md) | Goals, journeys, boundary |
| [Architecture](docs/architecture.md) | Control/data plane |
| [Bootstrap](docs/runbooks/bootstrap.md) | Install and first config |
| [Operations](docs/runbooks/operations.md) | Apply, secrets, incidents |
| [Decision log](docs/decision-log.md) | Confirmed product decisions |
| [Design index](docs/README.md) | Full reading order |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for setup, PR expectations, and release flow.
Please follow the [Code of Conduct](CODE_OF_CONDUCT.md).

## License

Licensed under the [Apache License, Version 2.0](LICENSE).
