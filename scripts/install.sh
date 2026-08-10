#!/bin/sh
# ProxyCore install / update
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/zerkc/ProxyCore/main/scripts/install.sh | sh
#
# Optional environment:
#   PROXYCORE_HOME     Install directory (default: /opt/proxycore as root, else ~/proxycore)
#   PROXYCORE_REPO     Git remote (default: https://github.com/zerkc/ProxyCore.git)
#   PROXYCORE_BRANCH   Git branch (default: main)
#   WEB_PORT           Published control-plane port (default: 3000)
#   DNS_PORT           Published CoreDNS port (default: 53)
#   SKIP_BUILD=1       Skip image rebuild (update config/migrate only)
#
# The script is idempotent: first run installs, later runs pull + migrate + recreate.

set -eu

PROXYCORE_REPO="${PROXYCORE_REPO:-https://github.com/zerkc/ProxyCore.git}"
PROXYCORE_BRANCH="${PROXYCORE_BRANCH:-main}"
WEB_PORT="${WEB_PORT:-3000}"
DNS_PORT="${DNS_PORT:-53}"
SKIP_BUILD="${SKIP_BUILD:-0}"

if [ -z "${PROXYCORE_HOME:-}" ]; then
  if [ "$(id -u)" -eq 0 ]; then
    PROXYCORE_HOME=/opt/proxycore
  else
    PROXYCORE_HOME="${HOME}/proxycore"
  fi
fi

log() {
  printf '==> %s\n' "$*"
}

warn() {
  printf 'warning: %s\n' "$*" >&2
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

random_b64() {
  # 32 raw bytes → base64 (master key / passwords)
  openssl rand -base64 32 | tr -d '\n'
}

random_password() {
  openssl rand -base64 24 | tr -d '/+=\n' | cut -c1-32
}

wait_postgres() {
  attempts=60
  while [ "$attempts" -gt 0 ]; do
    if docker compose exec -T postgres pg_isready -U "${POSTGRES_USER:-proxycore}" -d "${POSTGRES_DB:-proxycore}" >/dev/null 2>&1; then
      return 0
    fi
    attempts=$((attempts - 1))
    sleep 1
  done
  die "postgres did not become ready"
}

# Append KEY=VALUE to .env when the key is missing (preserves existing values).
ensure_env_key() {
  key="$1"
  value="$2"
  if ! grep -q "^${key}=" .env 2>/dev/null; then
    printf '%s=%s\n' "$key" "$value" >>.env
    log "Added ${key} to .env"
  fi
}

ensure_env() {
  if [ -f .env ]; then
    log "Keeping existing .env"
    ensure_env_key NGINX_ACME_UPSTREAM "http://127.0.0.1:${WEB_PORT}"
    # Older installs omitted publish ports; Compose needs these for CoreDNS/API.
    ensure_env_key WEB_PORT "${WEB_PORT}"
    ensure_env_key DNS_PORT "${DNS_PORT}"
    return 0
  fi

  need_cmd openssl
  master_key="$(random_b64)"
  postgres_password="$(random_password)"

  cat >.env <<EOF
PROXYCORE_MASTER_KEY_BASE64=${master_key}
POSTGRES_DB=proxycore
POSTGRES_USER=proxycore
POSTGRES_PASSWORD=${postgres_password}
WEB_PORT=${WEB_PORT}
DNS_PORT=${DNS_PORT}
PROXY_INGRESS_IPV4=
PROXY_INGRESS_IPV6=
ACME_DIRECTORY_URL=https://acme-staging-v02.api.letsencrypt.org/directory
ACME_PRODUCTION_DIRECTORY_URL=https://acme-v02.api.letsencrypt.org/directory
PROXYCORE_ACME_EMAIL=
PROXYCORE_CERT_RENEWAL_INTERVAL=1h
NGINX_ACME_UPSTREAM=http://127.0.0.1:${WEB_PORT}
EOF
  chmod 600 .env
  log "Created .env (generated PROXYCORE_MASTER_KEY_BASE64 and POSTGRES_PASSWORD)"
}

sync_repo() {
  if [ -d "${PROXYCORE_HOME}/.git" ]; then
    log "Updating ${PROXYCORE_HOME} (${PROXYCORE_BRANCH})"
    git -C "$PROXYCORE_HOME" remote set-url origin "$PROXYCORE_REPO" 2>/dev/null || true
    git -C "$PROXYCORE_HOME" fetch --depth 1 origin "$PROXYCORE_BRANCH"
    git -C "$PROXYCORE_HOME" checkout -q "$PROXYCORE_BRANCH"
    git -C "$PROXYCORE_HOME" reset --hard "origin/${PROXYCORE_BRANCH}"
    return 0
  fi

  if [ -e "$PROXYCORE_HOME" ] && [ "$(ls -A "$PROXYCORE_HOME" 2>/dev/null || true)" ]; then
    die "${PROXYCORE_HOME} exists but is not a ProxyCore git checkout"
  fi

  log "Installing ProxyCore into ${PROXYCORE_HOME}"
  parent="$(dirname "$PROXYCORE_HOME")"
  mkdir -p "$parent"
  git clone --depth 1 --branch "$PROXYCORE_BRANCH" "$PROXYCORE_REPO" "$PROXYCORE_HOME"
}

check_host_ports() {
  # Best-effort conflict detection for nginx host networking + CoreDNS publish.
  for port in 80 443 "$DNS_PORT"; do
    if command -v ss >/dev/null 2>&1; then
      if ss -lntu 2>/dev/null | grep -Eq ":${port}([[:space:]]|$)"; then
        if [ "$port" = "$DNS_PORT" ]; then
          warn "port ${port} appears in use; CoreDNS may fail to publish (disable systemd-resolved DNSStubListener or set DNS_PORT)"
        else
          warn "port ${port} appears in use; nginx host mode may fail to bind"
        fi
      fi
    fi
  done
}

verify_coredns_published() {
  # Confirm the CoreDNS service is up and the host publish mapping exists.
  if ! docker compose ps --status running --services 2>/dev/null | grep -qx coredns; then
    die "CoreDNS is not running; check: docker compose logs coredns"
  fi
  ports="$(docker compose ps coredns --format '{{.Ports}}' 2>/dev/null || true)"
  case "$ports" in
  *":${DNS_PORT}->53/"* | *"0.0.0.0:${DNS_PORT}->53/"* | *"[::]:${DNS_PORT}->53/"*)
    return 0
    ;;
  esac
  # Fallback: inspect published bindings (format varies by Compose version).
  if docker compose exec -T coredns true >/dev/null 2>&1; then
    if docker port proxycore-coredns 53/udp 2>/dev/null | grep -Eq ":${DNS_PORT}\$" \
      || docker port proxycore-coredns 53/tcp 2>/dev/null | grep -Eq ":${DNS_PORT}\$"; then
      return 0
    fi
  fi
  die "CoreDNS is running but host port ${DNS_PORT} is not published (ports=${ports:-none})"
}

main() {
  need_cmd git
  need_cmd docker
  docker compose version >/dev/null 2>&1 || die "Docker Compose v2 is required (docker compose)"
  docker info >/dev/null 2>&1 || die "Docker daemon is not reachable (is the user in the docker group?)"

  sync_repo
  cd "$PROXYCORE_HOME"
  ensure_env

  # Export compose project name for stable container names across updates.
  export COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME:-proxycore}"

  # shellcheck disable=SC1091
  set -a
  # Load .env so wait_postgres / publish ports see file values.
  . ./.env
  set +a
  # Compose reads .env from disk; also export publish ports for this process.
  WEB_PORT="${WEB_PORT:-3000}"
  DNS_PORT="${DNS_PORT:-53}"
  export WEB_PORT DNS_PORT

  check_host_ports

  log "Starting PostgreSQL"
  docker compose up -d postgres
  wait_postgres

  log "Applying database migrations"
  docker compose --profile tools run --rm migrate

  if [ "$SKIP_BUILD" = "1" ]; then
    log "Recreating services (no rebuild)"
    docker compose up -d --remove-orphans
  else
    log "Building and starting services (Go API + Vite SPA; nginx host network)"
    docker compose up -d --build --remove-orphans
  fi

  log "Verifying CoreDNS is published on host port ${DNS_PORT}"
  verify_coredns_published

  log "ProxyCore is up"
  printf '\n'
  printf '  Home:      %s\n' "$PROXYCORE_HOME"
  printf '  Dashboard: http://<host-ip>:%s\n' "${WEB_PORT}"
  printf '  Bootstrap: http://<host-ip>:%s/bootstrap\n' "${WEB_PORT}"
  printf '  DNS:       <host-ip>:%s (CoreDNS TCP/UDP)\n' "${DNS_PORT}"
  printf '  Nginx:     host network (80/443 + stream ports)\n'
  printf '\n'
  printf 'First install: open /bootstrap once to create the Owner.\n'
  printf 'Update again with the same curl | sh command.\n'
}

main "$@"
