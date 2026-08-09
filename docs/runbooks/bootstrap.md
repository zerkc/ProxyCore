# ProxyCore Bootstrap Runbook

## One-line install / update

On a Linux host with Docker Engine + Compose v2 and Git:

```sh
curl -fsSL https://raw.githubusercontent.com/zerkc/ProxyCore/main/scripts/install.sh | sh
```

The script clones or updates the checkout (default `/opt/proxycore` as root,
otherwise `~/proxycore`), creates `.env` with generated secrets on first run,
starts PostgreSQL, applies migrations, and brings the stack up with **Nginx in
`network_mode: host`** so HTTP/HTTPS and stream ports bind directly on the host.

Optional environment variables before `| sh`:

| Variable | Default | Purpose |
| --- | --- | --- |
| `PROXYCORE_HOME` | `/opt/proxycore` or `~/proxycore` | Install directory |
| `PROXYCORE_BRANCH` | `main` | Git branch |
| `WEB_PORT` | `3000` | Control-plane port |
| `DNS_PORT` | `53` | CoreDNS publish port |
| `SKIP_BUILD` | `0` | Set `1` to recreate without rebuild |

Re-run the same command to update: pull, migrate, rebuild/recreate. `.env` is
preserved.

## Manual preconditions

1. Install Docker Engine with Compose v2.
2. Create a private `.env` from `.env.example` (skipped when using `install.sh`).
3. Generate a 32-byte master key outside the repository:

   ```sh
   openssl rand -base64 32
   ```

4. Set `PROXYCORE_MASTER_KEY_BASE64` and change the PostgreSQL password.
5. Confirm host ports 80 and 443 are free for Nginx host networking. HTTP/3
   additionally requires both TCP and UDP 443. CoreDNS publishes `DNS_PORT`
   (default 53).

## Manual start

```sh
docker compose config
docker compose up -d postgres
docker compose run --rm web pnpm db:migrate
docker compose up -d --build
```

Open `/bootstrap` once and create the first Owner. The bootstrap endpoint must
reject every later attempt.

## First configuration

1. Open the dashboard through the host's LAN address when possible; ProxyCore
   automatically persists that address for proxied DNS answers. Set or override
   it in Network settings when the detected address is not the intended LAN,
   NAT, or public address.
2. Set the default resolver pool and any more-specific suffix pools.
3. Add a managed zone and typed records; each save queues an apply immediately.
4. In Streams/Network, issue a self-signed certificate for the proxied hostname.
5. Create a proxied record. For Compose smoke tests, point the origin at the
   demo upstream `172.30.0.10:80` (service `demo-upstream`). Configure port,
   redirects, and Basic Auth from the Record dialog as needed.
6. Review the resulting job status; use manual apply only to re-apply the
   current desired state.

Invalid desired state must never replace the last applied revision.

## Compose smoke test

Nginx listens on the host network namespace. On Linux, the demo upstream at
`172.30.0.10:80` is usually reachable from host-mode Nginx via the Compose
`data` bridge.

```sh
# After bootstrap + network settings + zone
# 1) Issue cert for app.home.arpa in the Certificates view
# 2) Add proxied A record app → any DNS value, origin 172.30.0.10:80
# 3) Wait for apply, then:
curl -vk --resolve app.home.arpa:443:127.0.0.1 https://app.home.arpa/
```

Basic Auth, when enabled, requires the username/password from the Record dialog
and client HTTPS. Auth and certificate files are written into the shared
`candidates` volume next to the rendered Nginx config.
