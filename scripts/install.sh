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
#   ENROLLMENT_PORT    Published direct TLS enrollment port (default: 3443;
#                      not Nginx proxy ingress)
#   DNS_PORT           Published CoreDNS port (default: 53)
#   SKIP_BUILD=1       Skip image rebuild (update config only)
#   PROXYCORE_SKIP_MEMORY_CHECK=1
#                      Silence the low-memory/swap warning shown before builds
#   BUILDX_NO_DEFAULT_ATTESTATIONS
#                      Defaults to 1 for the build phase (no provenance/SBOM
#                      attestations); set it to 0 to keep them
#
# The script is idempotent: first run installs, later runs pull + recreate. The
# api applies SQLite schema migrations on start. Installs that still hold a
# legacy PostgreSQL volume are never converted automatically: the script stops
# and points at scripts/convert-postgres.sh.

set -eu

PROXYCORE_REPO="${PROXYCORE_REPO:-https://github.com/zerkc/ProxyCore.git}"
PROXYCORE_BRANCH="${PROXYCORE_BRANCH:-main}"
WEB_PORT_EXTERNAL_SET="${WEB_PORT+x}"
ENROLLMENT_PORT_EXTERNAL_SET="${ENROLLMENT_PORT+x}"
DNS_PORT_EXTERNAL_SET="${DNS_PORT+x}"
WEB_PORT="${WEB_PORT-3000}"
ENROLLMENT_PORT="${ENROLLMENT_PORT-3443}"
DNS_PORT="${DNS_PORT-53}"
# Precedence is explicit process environment, then safe .env data, then defaults.
SKIP_BUILD="${SKIP_BUILD:-0}"
# Images are built one at a time (never concurrently) so peak memory stays at a
# single image build, which matters on 1 GB hosts. These are the services with
# a build section in compose.yaml, excluding the profile-gated `migrate`. The
# shared runtime image is built once through `worker`; `control` only reuses it.
# Order is fixed and documented: small images first, the heavy api (UI + Go) and
# worker runtime images last. scripts/install.test.sh fails when this list
# drifts from compose.yaml.
BUILD_SERVICES="coredns nginx updater-bootstrap updater api worker"
# Low-memory guard thresholds, in kB (as reported by /proc/meminfo).
LOW_MEMORY_KB=1572864
LOW_SWAP_KB=1048576
# SQLite database path inside the api image (compose sqlite-data volume).
SQLITE_DB_PATH=/var/lib/proxycore-db/proxycore.db

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

validate_host_port() {
  name="$1"
  port="$2"
  case "$port" in
  ''|*[!0-9]*|??????*) die "${name} must be a numeric host port between 1 and 65535" ;;
  esac
  if [ "$port" -lt 1 ] || [ "$port" -gt 65535 ]; then
    die "${name} must be a numeric host port between 1 and 65535"
  fi
}

validate_runtime_values() {
  validate_host_port WEB_PORT "$WEB_PORT"
  validate_host_port ENROLLMENT_PORT "$ENROLLMENT_PORT"
  validate_host_port DNS_PORT "$DNS_PORT"
}

trim_horizontal() {
  trim_value="$1"
  while :; do
    case "$trim_value" in
      [[:blank:]]*) trim_value=${trim_value#?} ;;
      *) break ;;
    esac
  done
  while :; do
    case "$trim_value" in
      *[[:blank:]]) trim_value=${trim_value%?} ;;
      *) break ;;
    esac
  done
  TRIMMED_VALUE=$trim_value
}

parse_quoted_or_plain() {
  parse_key="$1"
  parse_value="$2"
  ENV_SINGLE_QUOTE="'"
  ENV_DOUBLE_QUOTE='"'
  trim_horizontal "$parse_value"
  parse_value=$TRIMMED_VALUE
  case "$parse_value" in
    "$ENV_SINGLE_QUOTE"*)
      case "$parse_value" in
        *"$ENV_SINGLE_QUOTE")
          PARSED_VALUE=${parse_value#?}
          PARSED_VALUE=${PARSED_VALUE%?}
          ;;
        *) die "malformed ${parse_key} in .env" ;;
      esac
      ;;
    "$ENV_DOUBLE_QUOTE"*)
      case "$parse_value" in
        *"$ENV_DOUBLE_QUOTE")
          PARSED_VALUE=${parse_value#?}
          PARSED_VALUE=${PARSED_VALUE%?}
          ;;
        *) die "malformed ${parse_key} in .env" ;;
      esac
      ;;
    *) PARSED_VALUE=$parse_value ;;
  esac
}

parse_quoted_port_value() {
  port_key="$1"
  port_raw="$2"
  ENV_SINGLE_QUOTE="'"
  ENV_DOUBLE_QUOTE='"'
  case "$port_raw" in
    "$ENV_SINGLE_QUOTE"*) port_quote=$ENV_SINGLE_QUOTE ;;
    "$ENV_DOUBLE_QUOTE"*) port_quote=$ENV_DOUBLE_QUOTE ;;
    *) die "malformed ${port_key} in .env" ;;
  esac
  port_rest=${port_raw#?}
  case "$port_rest" in
    *"$port_quote"*) ;;
    *) die "malformed ${port_key} in .env" ;;
  esac
  port_inner=${port_rest%%"$port_quote"*}
  port_suffix=${port_rest#"$port_inner$port_quote"}
  case "$port_suffix" in
    '') ;;
    [[:blank:]]*)
      trim_horizontal "$port_suffix"
      port_suffix=$TRIMMED_VALUE
      case "$port_suffix" in
        ''|\#*) ;;
        *) die "malformed ${port_key} in .env" ;;
      esac
      ;;
    *) die "malformed ${port_key} in .env" ;;
  esac
  PARSED_VALUE=$port_inner
}

parse_port_value() {
  port_key="$1"
  port_raw="$2"
  ENV_SINGLE_QUOTE="'"
  ENV_DOUBLE_QUOTE='"'
  trim_horizontal "$port_raw"
  port_raw=$TRIMMED_VALUE
  case "$port_raw" in
    "$ENV_SINGLE_QUOTE"*|"$ENV_DOUBLE_QUOTE"*)
      parse_quoted_port_value "$port_key" "$port_raw"
      ;;
    *)
      case "$port_raw" in
        *[[:blank:]]#*) port_raw=${port_raw%%[[:blank:]]#*} ;;
      esac
      parse_quoted_or_plain "$port_key" "$port_raw"
      ;;
  esac
  validate_host_port "$port_key" "$PARSED_VALUE"
}

load_env_values() {
  env_file="$1"
  env_line_number=0
  env_previous_key=''
  env_cr=$(printf '\r')
  ENV_WEB_PORT_FOUND=0
  ENV_ENROLLMENT_PORT_FOUND=0
  ENV_DNS_PORT_FOUND=0
  ENV_WEB_PORT_VALUE=''
  ENV_ENROLLMENT_PORT_VALUE=''
  ENV_DNS_PORT_VALUE=''

  if [ "$env_file" = "-" ]; then
    env_file=/dev/stdin
  fi
  while IFS= read -r env_line || [ -n "$env_line" ]; do
    env_line_number=$((env_line_number + 1))
    case "$env_line" in
      *"$env_cr") env_line=${env_line%?} ;;
    esac
    trim_horizontal "$env_line"
    env_line=$TRIMMED_VALUE
    case "$env_line" in
      ''|'#'*) env_previous_key='' ; continue ;;
    esac
    case "$env_line" in
      *=*) ;;
      *)
        case "$env_line" in
          *[!A-Za-z0-9_]*) ;;
          [A-Za-z_]* ) env_previous_key='' ; continue ;;
        esac
        if [ -n "$env_previous_key" ]; then
          die "invalid continuation in .env at line ${env_line_number}"
        fi
        continue
        ;;
    esac
    env_key=${env_line%%=*}
    env_value=${env_line#*=}
    trim_horizontal "$env_key"
    env_key=$TRIMMED_VALUE
    case "$env_key" in
      WEB_PORT|ENROLLMENT_PORT|DNS_PORT) ;;
      *) env_previous_key='' ; continue ;;
    esac
    case "$env_key" in
      WEB_PORT)
        [ "$ENV_WEB_PORT_FOUND" -eq 0 ] || die "duplicate WEB_PORT in .env"
        parse_port_value "$env_key" "$env_value"
        ENV_WEB_PORT_FOUND=1
        ENV_WEB_PORT_VALUE=$PARSED_VALUE
        ;;
      ENROLLMENT_PORT)
        [ "$ENV_ENROLLMENT_PORT_FOUND" -eq 0 ] || die "duplicate ENROLLMENT_PORT in .env"
        parse_port_value "$env_key" "$env_value"
        ENV_ENROLLMENT_PORT_FOUND=1
        ENV_ENROLLMENT_PORT_VALUE=$PARSED_VALUE
        ;;
      DNS_PORT)
        [ "$ENV_DNS_PORT_FOUND" -eq 0 ] || die "duplicate DNS_PORT in .env"
        parse_port_value "$env_key" "$env_value"
        ENV_DNS_PORT_FOUND=1
        ENV_DNS_PORT_VALUE=$PARSED_VALUE
        ;;
    esac
    env_previous_key=$env_key
  done < "$env_file"
}

env_key_present() {
  env_key_to_find="$1"
  env_key_file="$2"
  env_key_cr=$(printf '\r')
  while IFS= read -r env_key_line || [ -n "$env_key_line" ]; do
    case "$env_key_line" in
      *"$env_key_cr") env_key_line=${env_key_line%?} ;;
    esac
    trim_horizontal "$env_key_line"
    env_key_line=$TRIMMED_VALUE
    case "$env_key_line" in
      *=*) ;;
      *) continue ;;
    esac
    env_key_candidate=${env_key_line%%=*}
    trim_horizontal "$env_key_candidate"
    [ "$TRIMMED_VALUE" = "$env_key_to_find" ] && return 0
  done < "$env_key_file"
  return 1
}

apply_env_file_values() {
  if [ "$WEB_PORT_EXTERNAL_SET" != x ] && [ "$ENV_WEB_PORT_FOUND" -eq 1 ]; then
    WEB_PORT=$ENV_WEB_PORT_VALUE
  fi
  if [ "$ENROLLMENT_PORT_EXTERNAL_SET" != x ] && [ "$ENV_ENROLLMENT_PORT_FOUND" -eq 1 ]; then
    ENROLLMENT_PORT=$ENV_ENROLLMENT_PORT_VALUE
  fi
  if [ "$DNS_PORT_EXTERNAL_SET" != x ] && [ "$ENV_DNS_PORT_FOUND" -eq 1 ]; then
    DNS_PORT=$ENV_DNS_PORT_VALUE
  fi
}

assert_env_file_regular() {
  env_file="$1"
  if [ -L "$env_file" ]; then
    die ".env must not be a symlink"
  fi
  if [ -e "$env_file" ] && [ ! -f "$env_file" ]; then
    die ".env must be a regular file"
  fi
}

source_env_helper() {
  env_helper="$1"
  if [ "${INSTALL_ENV_HELPER_LOADED:-0}" = 1 ]; then
    return 0
  fi
  if [ ! -f "$env_helper" ]; then
    die "trusted installer environment helper is missing"
  fi
  . "$env_helper"
}

preflight_runtime_values() {
  env_file="$1"
  validate_runtime_values
  if [ "$env_file" = "-" ]; then
    load_env_values -
    apply_env_file_values
  else
    assert_env_file_regular "$env_file"
    if [ -f "$env_file" ]; then
      load_env_values "$env_file"
      apply_env_file_values
    fi
  fi
  validate_runtime_values
}

random_b64() {
  # 32 raw bytes → base64 (master key)
  openssl rand -base64 32 | tr -d '\n'
}

# A legacy PostgreSQL install is detected by its named volume. It is only a
# problem while no SQLite database exists yet: the converter never runs here
# automatically and the volume is never touched.
# The compose project decides the legacy volume name. It comes from the
# environment, then from the install .env (read as data, never sourced or
# printed; a missing .env is fine), then the default project.
compose_project_name() {
  project_name="${COMPOSE_PROJECT_NAME:-}"
  if [ -z "$project_name" ] && [ -n "${PROXYCORE_HOME:-}" ] && [ -f "${PROXYCORE_HOME}/.env" ]; then
    project_name=$(tr -d '\r' < "${PROXYCORE_HOME}/.env" \
      | sed -n 's/^[[:space:]]*COMPOSE_PROJECT_NAME[[:space:]]*=[[:space:]]*//p' \
      | sed -n '1p' \
      | sed -e 's/^"\(.*\)"$/\1/' -e "s/^'\(.*\)'\$/\1/")
  fi
  printf '%s\n' "${project_name:-proxycore}"
}

legacy_postgres_unconverted() {
  legacy_volume="$(compose_project_name)_postgres-data"
  docker volume inspect "$legacy_volume" >/dev/null 2>&1 || return 1
  if docker compose run --rm --no-deps -T --entrypoint test api -f "$SQLITE_DB_PATH" >/dev/null 2>&1; then
    return 1
  fi
  return 0
}

refuse_unconverted_legacy_postgres() {
  if legacy_postgres_unconverted; then
    die "legacy PostgreSQL data found (volume $(compose_project_name)_postgres-data) and no SQLite database exists yet. Nothing was changed. Convert it offline first: cd ${PROXYCORE_HOME} && sh scripts/convert-postgres.sh (see docs/runbooks/upgrade-from-postgres.md), then run this installer again"
  fi
}

# Append KEY=VALUE to .env when the key is missing (preserves existing values).
ensure_env_key() {
  key="$1"
  value="$2"
  assert_env_file_regular .env
  if ! env_key_present "$key" .env; then
    atomic_append_env_key .env "$key" "$value"
    log "Added ${key} to .env"
  fi
}

ensure_env() {
  assert_env_file_regular .env
  if [ -f .env ]; then
    log "Keeping existing .env"
    ensure_env_key NGINX_ACME_UPSTREAM "http://127.0.0.1:${WEB_PORT}"
    # Older installs omitted publish ports; Compose needs these for CoreDNS/API.
    ensure_env_key WEB_PORT "${WEB_PORT}"
    ensure_env_key ENROLLMENT_PORT "${ENROLLMENT_PORT}"
    ensure_env_key DNS_PORT "${DNS_PORT}"
    return 0
  fi

  need_cmd openssl
  master_key="$(random_b64)"
  create_env_file .env
  log "Created .env (generated PROXYCORE_MASTER_KEY_BASE64)"
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
  # Best-effort conflict detection for nginx host networking, CoreDNS, and
  # the direct TLS enrollment publish. Existing installs receive warnings,
  # rather than a new hard failure, so update behavior stays compatible.
  for port in 80 443 "$DNS_PORT" "$ENROLLMENT_PORT"; do
    if command -v ss >/dev/null 2>&1; then
      if ss -lntu 2>/dev/null | grep -Eq ":${port}([[:space:]]|$)"; then
        if [ "$port" = "$DNS_PORT" ]; then
          warn "port ${port} appears in use; CoreDNS may fail to publish (disable systemd-resolved DNSStubListener or set DNS_PORT)"
        elif [ "$port" = "$ENROLLMENT_PORT" ]; then
          warn "port ${port} appears in use; direct TLS enrollment may fail to bind (set ENROLLMENT_PORT)"
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

# Warn (never fail, never change the system) when the host is likely to be slow
# or OOM-killed while building. Silent where /proc/meminfo does not exist.
check_build_memory() {
  [ "${PROXYCORE_SKIP_MEMORY_CHECK:-0}" = "1" ] && return 0
  meminfo="${PROXYCORE_MEMINFO_PATH:-/proc/meminfo}"
  [ -r "$meminfo" ] || return 0
  mem_kb=$(awk '/^MemTotal:/ { print $2; exit }' "$meminfo" 2>/dev/null || true)
  swap_kb=$(awk '/^SwapTotal:/ { print $2; exit }' "$meminfo" 2>/dev/null || true)
  case "$mem_kb" in ''|*[!0-9]*) return 0 ;; esac
  case "$swap_kb" in ''|*[!0-9]*) swap_kb=0 ;; esac
  if [ "$mem_kb" -lt "$LOW_MEMORY_KB" ] && [ "$swap_kb" -lt "$LOW_SWAP_KB" ]; then
    warn "low memory host (RAM $((mem_kb / 1024)) MB, swap $((swap_kb / 1024)) MB): image builds may be very slow or be killed by the OOM killer. Add at least 1 GB of swap first, for example: sudo fallocate -l 1G /swapfile && sudo chmod 600 /swapfile && sudo mkswap /swapfile && sudo swapon /swapfile (this script never changes swap). Set PROXYCORE_SKIP_MEMORY_CHECK=1 to silence this warning"
  fi
  return 0
}

# Build every service in BUILD_SERVICES one at a time with progress output.
build_services() {
  : "${BUILDX_NO_DEFAULT_ATTESTATIONS:=1}"
  export BUILDX_NO_DEFAULT_ATTESTATIONS
  build_total=0
  for svc in $BUILD_SERVICES; do
    build_total=$((build_total + 1))
  done
  build_index=0
  for svc in $BUILD_SERVICES; do
    build_index=$((build_index + 1))
    log "[${build_index}/${build_total}] building ${svc}"
    build_start=$(date +%s)
    if ! docker compose build "$svc"; then
      die "build of service '${svc}' failed; nothing was started. Retry with: docker compose build ${svc} (then run this installer again)"
    fi
    build_end=$(date +%s)
    log "[${build_index}/${build_total}] built ${svc} in $((build_end - build_start))s"
  done
}

deploy_stack() {
  if [ "$SKIP_BUILD" = "1" ]; then
    log "Recreating services (no rebuild)"
    docker compose up -d --remove-orphans
  else
    check_build_memory
    log "Building images one at a time (Go API + Vite SPA; first build can take a long time on small hosts)"
    build_services
    log "Starting services (nginx host network)"
    docker compose up -d --remove-orphans
  fi
}

main() {
  validate_runtime_values

  if [ -z "${PROXYCORE_HOME:-}" ]; then
    if [ "$(id -u)" -eq 0 ]; then
      PROXYCORE_HOME=/opt/proxycore
    else
      PROXYCORE_HOME="${HOME}/proxycore"
    fi
  fi
  preflight_runtime_values "${PROXYCORE_HOME}/.env"

  need_cmd git
  need_cmd docker
  need_cmd mktemp
  docker compose version >/dev/null 2>&1 || die "Docker Compose v2 is required (docker compose)"
  docker info >/dev/null 2>&1 || die "Docker daemon is not reachable (is the user in the docker group?)"

  sync_repo
  cd "$PROXYCORE_HOME"
  source_env_helper ./scripts/install_env.sh
  validate_runtime_values
  ensure_env

  # The updater sidecar invokes Docker Compose through the host socket. Give
  # it the real host project path so relative bind mounts resolve on the host,
  # not inside the sidecar container.
  export PROXYCORE_HOST_HOME="$(pwd)"

  # Export compose project name for stable container names across updates. The
  # same resolution (environment, .env, default) drives the legacy volume check.
  COMPOSE_PROJECT_NAME="$(compose_project_name)"
  export COMPOSE_PROJECT_NAME

  # Compose reads .env itself. Export only the validated values this script
  # needs so explicit process environment values retain precedence.
  export WEB_PORT ENROLLMENT_PORT DNS_PORT

  check_host_ports

  log "Checking for unconverted legacy PostgreSQL data"
  refuse_unconverted_legacy_postgres

  deploy_stack

  log "Verifying CoreDNS is published on host port ${DNS_PORT}"
  verify_coredns_published

  log "ProxyCore is up"
  printf '\n'
  printf '  Home:      %s\n' "$PROXYCORE_HOME"
  printf '  Dashboard: http://<host-ip>:%s\n' "${WEB_PORT}"
  printf '  Bootstrap: http://<host-ip>:%s/bootstrap\n' "${WEB_PORT}"
  printf '  DNS:       <host-ip>:%s (CoreDNS TCP/UDP)\n' "${DNS_PORT}"
  printf '  Enrollment: https://<host-ip>:%s (direct TLS enrollment; not Nginx proxy ingress; listener startup pending)\n' "${ENROLLMENT_PORT}"
  printf '  Nginx:     host network (80/443 + stream ports)\n'
  printf '\n'
  printf 'First install: open /bootstrap once to create the Owner.\n'
  printf 'Update again with the same curl | sh command.\n'
}

if [ "${PROXYCORE_INSTALL_NO_MAIN:-0}" != "1" ]; then
  main "$@"
fi
