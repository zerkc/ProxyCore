# ProxyCore

Single-host homelab control plane for **local DNS** and **secure ingress**.

Manage zones and records in a Cloudflare-style UI, toggle proxy on A/AAAA/CNAME, issue certificates, and forward TCP/UDP streams — without hand-editing CoreDNS or Nginx.

## What it does

| Job | Behavior |
| --- | --- |
| Sole homelab DNS | CoreDNS is authoritative for managed zones and forwards everything else to your resolvers (Pi-hole, AdGuard, public DNS, …). |
| Cloudflare-style proxy | Proxied records resolve to this host’s ingress address; Nginx terminates TLS and forwards to an explicit origin IP:port. |
| Certificates | Self-signed, Let’s Encrypt HTTP-01, or Let’s Encrypt DNS-01 via Cloudflare (`_acme-challenge` only). |
| Streams | Separate TCP/UDP listeners → upstream IP:port (Nginx stream). |
| Safe apply | Desired state in PostgreSQL → worker renders → validates → reloads → rolls back on failure. |
| Local operators | One-time Owner bootstrap; Owner/Operator roles; no OIDC/MFA in the MVP. |

Control plane (Next.js + worker) never gets a raw Docker socket. A small helper applies fixed Nginx/CoreDNS operations over a private Unix socket.

## Quick install / update

Linux host with Docker Engine (Compose v2) and Git:

```sh
curl -fsSL https://raw.githubusercontent.com/zerkc/ProxyCore/main/scripts/install.sh | sh
```

That clones or updates the checkout (`/opt/proxycore` as root, otherwise `~/proxycore`), creates `.env` with generated secrets on first run, applies migrations, and starts the stack. **Nginx runs with `network_mode: host`** so HTTP/HTTPS and stream ports bind on the host.

Then open `http://<host-ip>:3000/bootstrap` once to create the Owner.

| Variable | Default | Purpose |
| --- | --- | --- |
| `PROXYCORE_HOME` | `/opt/proxycore` or `~/proxycore` | Install directory |
| `PROXYCORE_BRANCH` | `main` | Git branch |
| `WEB_PORT` | `3000` | Dashboard / API |
| `DNS_PORT` | `53` | CoreDNS |
| `SKIP_BUILD` | `0` | Set `1` to recreate without rebuild |

Details: [docs/runbooks/bootstrap.md](docs/runbooks/bootstrap.md).

## Stack

Docker Compose on one Linux host:

- **web** — Next.js admin UI and control-plane API  
- **postgres** — desired state, users, jobs, audit  
- **worker** — render, validate, apply, health checks  
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
