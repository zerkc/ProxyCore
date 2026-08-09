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

ensure_env() {
  if [ -f .env ]; then
    log "Keeping existing .env"
    if ! grep -q '^NGINX_ACME_UPSTREAM=' .env; then
      printf '\nNGINX_ACME_UPSTREAM=http://127.0.0.1:%s\n' "$WEB_PORT" >>.env
    fi
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
  # Best-effort conflict detection for nginx host networking.
  for port in 80 443; do
    if command -v ss >/dev/null 2>&1; then
      if ss -lntu 2>/dev/null | grep -Eq ":${port}\\s"; then
        warn "port ${port} appears in use; nginx host mode may fail to bind"
      fi
    fi
  done
}

main() {
  need_cmd git
  need_cmd docker
  docker compose version >/dev/null 2>&1 || die "Docker Compose v2 is required (docker compose)"
  docker info >/dev/null 2>&1 || die "Docker daemon is not reachable (is the user in the docker group?)"

  sync_repo
  cd "$PROXYCORE_HOME"
  ensure_env
  check_host_ports

  # Export compose project name for stable container names across updates.
  export COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME:-proxycore}"

  # shellcheck disable=SC1091
  set -a
  # Load .env so wait_postgres sees POSTGRES_* defaults when overridden.
  . ./.env
  set +a

  log "Starting PostgreSQL"
  docker compose up -d postgres
  wait_postgres

  log "Applying database migrations"
  docker compose run --rm --no-deps web pnpm db:migrate

  if [ "$SKIP_BUILD" = "1" ]; then
    log "Recreating services (no rebuild)"
    docker compose up -d --remove-orphans
  else
    log "Building and starting services (nginx network_mode=host)"
    docker compose up -d --build --remove-orphans
  fi

  log "ProxyCore is up"
  printf '\n'
  printf '  Home:      %s\n' "$PROXYCORE_HOME"
  printf '  Dashboard: http://<host-ip>:%s\n' "${WEB_PORT}"
  printf '  Bootstrap: http://<host-ip>:%s/bootstrap\n' "${WEB_PORT}"
  printf '  DNS:       <host-ip>:%s\n' "${DNS_PORT}"
  printf '  Nginx:     host network (80/443 + stream ports)\n'
  printf '\n'
  printf 'First install: open /bootstrap once to create the Owner.\n'
  printf 'Update again with the same curl | sh command.\n'
}

main "$@"
