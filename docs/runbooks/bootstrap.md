# ProxyCore Bootstrap Runbook

## One-line install / update

On a Linux host with Docker Engine + Compose v2 and Git:

```sh
curl -fsSL https://raw.githubusercontent.com/zerkc/ProxyCore/main/scripts/install.sh | sh
```

The script clones or updates the checkout (default `/opt/proxycore` as root,
otherwise `~/proxycore`), creates `.env` with generated secrets on first run,
brings the stack up (the api creates and migrates the SQLite database on start) with the **Go
API + Vite SPA** control plane (no Next.js) and **Nginx in `network_mode: host`**
so HTTP/HTTPS and stream ports bind directly on the host.

Optional environment variables before `| sh`:

| Variable           | Default                           | Purpose                             |
| ------------------ | --------------------------------- | ----------------------------------- |
| `PROXYCORE_HOME`   | `/opt/proxycore` or `~/proxycore` | Install directory                   |
| `PROXYCORE_BRANCH` | `main`                            | Git branch                          |
| `WEB_PORT`         | `3000`                            | Control-plane port                  |
| `DNS_PORT`         | `53`                              | CoreDNS publish port                |
| `PROXYCORE_INSTALL_MODE` | `pull`                      | `pull` downloads prebuilt images; `build` compiles them on the host |
| `PROXYCORE_VERSION`      | `latest`                    | Image tag to pull (for example `0.5.4`) |
| `PROXYCORE_IMAGE_REPO`   | `ghcr.io/zerkc`             | Registry/owner the images are pulled from |
| `SKIP_BUILD`             | `0`                         | Set `1` to recreate without pulling or building |

Re-run the same command to update: pull images, recreate. `.env` is
preserved. If the installer finds a legacy PostgreSQL volume and no SQLite
database it stops without changing anything; follow
[upgrade-from-postgres.md](upgrade-from-postgres.md).

### Supported platforms

Release images are multi-arch manifests on GHCR
(`ghcr.io/zerkc/proxycore-<name>`), so Docker pulls the right variant itself.

| Host architecture            | Status                                                                 |
| ---------------------------- | ---------------------------------------------------------------------- |
| linux/amd64 (x86_64)         | Fully supported                                                        |
| linux/arm64 (aarch64)        | Fully supported (Raspberry Pi 3/4/5 with a 64-bit OS)                  |
| linux/arm/v7 (armv7l, armv6l)| Not supported: api, nginx, coredns, updater and migrate images exist, but the bun-based worker/control runtime does not (Bun has no 32-bit ARM build). The installer refuses to run; use a 64-bit OS. |
| linux/386 (i386, i686)       | Not supported; the installer refuses to run                            |

`PROXYCORE_ALLOW_UNSUPPORTED_ARCH=1` lets the installer continue on 32-bit ARM,
but the worker will not start there; it exists only for experiments.

### Install modes

- **pull (default)**: `docker compose pull` then `docker compose up -d`. No
  compiler or build cache is needed on the host, and the install takes minutes
  even on a Raspberry Pi. Pin a release with `PROXYCORE_VERSION=0.5.4`.
- **build (opt-in)**: `PROXYCORE_INSTALL_MODE=build` builds every image on the
  host, one at a time. It is install-only: dashboard updates always pull
  images. Use it for local development or forks that do not publish images (set
  `PROXYCORE_IMAGE_REPO` accordingly).

### Low-memory hosts (Raspberry Pi 3 B+ / 1 GB)

In pull mode nothing is compiled on the device, so a small host needs no swap
tuning for the install itself; this is the recommended path. The notes below only
apply to `PROXYCORE_INSTALL_MODE=build`.

In build mode the installer builds the images one at a time (`[n/N] building <service>`
with the elapsed seconds after each one), then starts the stack. Peak memory stays
at one image build, but the first build can still take a long time on a small host.

- Add at least 1 GB of swap before building. The installer only warns when RAM
  is below about 1.5 GB and swap below 1 GB; it never creates swap or uses sudo.
  Example: `sudo fallocate -l 1G /swapfile && sudo chmod 600 /swapfile && sudo mkswap /swapfile && sudo swapon /swapfile`
  (add the file to `/etc/fstab` to keep it after a reboot).
- Prefer a USB SSD over an SD card for the Docker data directory and the swap file.
- Set `PROXYCORE_SKIP_MEMORY_CHECK=1` to silence the warning.
- `SKIP_BUILD=1` recreates the services without pulling or building anything.
- A failed build stops the installer and names the service; retry it with
  `docker compose build <service>` and run the installer again.

### Dashboard self-updates

The dashboard update action always pulls the published images referenced by
`compose.yaml` (`PROXYCORE_VERSION`, default `latest`) and recreates the
services; no source is compiled and no source checkout is modified. Building
from source is only available at install time (`PROXYCORE_INSTALL_MODE=build`).
A leftover `PROXYCORE_UPDATE_MODE` value in `.env` is ignored.

During an update the dashboard locks the page and keeps a full-screen progress
dialog open through the service restart. Refresh the page only after the dialog
reports that the new version is live.

## Manual preconditions

1. Install Docker Engine with Compose v2.
2. Create a private `.env` from `.env.example` (skipped when using `install.sh`).
3. Generate a 32-byte master key outside the repository:

   ```sh
   openssl rand -base64 32
   ```

4. Set `PROXYCORE_MASTER_KEY_BASE64`.
5. Confirm host ports 80 and 443 are free for Nginx host networking. HTTP/3
   additionally requires both TCP and UDP 443. CoreDNS publishes `DNS_PORT`
   (default 53).

## Manual start

```sh
docker compose config
docker compose pull
docker compose up -d
```

To build from source instead, run `docker compose up -d --build`.

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
