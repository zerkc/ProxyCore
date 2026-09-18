#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)

contains() {
  file=$1
  expected=$2
  if ! grep -Fq -- "$expected" "$file"; then
    printf 'missing %s in %s\n' "$expected" "$file" >&2
    exit 1
  fi
}

# The API receives a fixed internal enrollment address while its host port is
# independently configurable from the dashboard/API port.
contains "$ROOT/compose.yaml" 'PROXYCORE_ENROLLMENT_TLS_ADDR: ":3443"'
contains "$ROOT/compose.yaml" '"${WEB_PORT:-3000}:3000"'
contains "$ROOT/compose.yaml" '"${ENROLLMENT_PORT:-3443}:3443"'
contains "$ROOT/infra/compose/Dockerfile.api" 'EXPOSE 3000 3443'

# Installer defaults and preflight checks keep legacy WEB_PORT behavior intact.
contains "$ROOT/scripts/install.sh" 'ENROLLMENT_PORT="${ENROLLMENT_PORT-3443}"'
contains "$ROOT/scripts/install.sh" 'ensure_env_key ENROLLMENT_PORT "${ENROLLMENT_PORT}"'
contains "$ROOT/scripts/install_env.sh" 'ENROLLMENT_PORT=${ENROLLMENT_PORT}'
contains "$ROOT/scripts/install.sh" 'source_env_helper ./scripts/install_env.sh'
contains "$ROOT/scripts/install.sh" '"$ENROLLMENT_PORT"'
contains "$ROOT/scripts/install.sh" 'direct TLS enrollment'
contains "$ROOT/scripts/install.sh" 'load_env_values()'
contains "$ROOT/scripts/install.sh" 'PROXYCORE_INSTALL_NO_MAIN'
if grep -Fq -- '. ./.env' "$ROOT/scripts/install.sh"; then
  printf 'installer must not source .env\n' >&2
  exit 1
fi

run_loader() {
  input=$1
  printf '%s' "$input" | (
    unset WEB_PORT ENROLLMENT_PORT DNS_PORT POSTGRES_USER POSTGRES_DB
    PROXYCORE_INSTALL_NO_MAIN=1 . "$ROOT/scripts/install.sh"
    load_env_values -
    apply_env_file_values
    printf '%s|%s|%s|%s|%s\n' \
      "$WEB_PORT" "$ENROLLMENT_PORT" "$DNS_PORT" "$POSTGRES_USER" "$POSTGRES_DB"
  )
}

assert_file_rejected() {
  label=$1
  input=$2
  output=''
  if output=$(printf '%s' "$input" | (
    unset WEB_PORT ENROLLMENT_PORT DNS_PORT POSTGRES_USER POSTGRES_DB
    PROXYCORE_INSTALL_NO_MAIN=1 . "$ROOT/scripts/install.sh"
    load_env_values -
    apply_env_file_values
    validate_host_port ENROLLMENT_PORT "$ENROLLMENT_PORT"
  ) 2>&1); then
    printf '%s was accepted\n' "$label" >&2
    exit 1
  fi
  case "$output" in
    *ENROLLMENT_SIDE_EFFECT*)
      printf '%s executed payload output\n' "$label" >&2
      exit 1
      ;;
  esac
}

assert_external_port_rejected() {
  variable=$1
  value=$2
  output=''
  if output=$(
    (
      case "$variable" in
        WEB_PORT) WEB_PORT=$value ;;
        ENROLLMENT_PORT) ENROLLMENT_PORT=$value ;;
        DNS_PORT) DNS_PORT=$value ;;
        *) exit 1 ;;
      esac
      PROXYCORE_INSTALL_NO_MAIN=1 . "$ROOT/scripts/install.sh"
      validate_host_port "$variable" "$value"
    ) 2>&1
  ); then
    printf '%s accepted unsafe environment value\n' "$variable" >&2
    exit 1
  fi
  case "$output" in
    *ENROLLMENT_SIDE_EFFECT*)
      printf '%s executed payload output\n' "$variable" >&2
      exit 1
      ;;
  esac
}

valid_env='WEB_PORT=3100
ENROLLMENT_PORT=9443
DNS_PORT=5353
POSTGRES_USER=proxycore
POSTGRES_DB=proxycore'
[ "$(run_loader "$valid_env")" = '3100|9443|5353|proxycore|proxycore' ] || {
  printf 'valid existing .env values were not loaded as data\n' >&2
  exit 1
}

external_env='WEB_PORT=3100
ENROLLMENT_PORT=9443
DNS_PORT=5353
POSTGRES_USER=file_user
POSTGRES_DB=file_db'
run_external_loader() {
  input=$1
  printf '%s' "$input" | (
    WEB_PORT=4100 ENROLLMENT_PORT=9444 DNS_PORT=5354
    POSTGRES_USER=external_user POSTGRES_DB=external_db
    PROXYCORE_INSTALL_NO_MAIN=1 . "$ROOT/scripts/install.sh"
    load_env_values -
    apply_env_file_values
    printf '%s|%s|%s|%s|%s\n' \
      "$WEB_PORT" "$ENROLLMENT_PORT" "$DNS_PORT" "$POSTGRES_USER" "$POSTGRES_DB"
  )
}
[ "$(run_external_loader "$external_env")" = '4100|9444|5354|external_user|external_db' ] || {
  printf 'external environment did not override .env data\n' >&2
  exit 1
}

for payload in \
  '$(printf 3443)' \
  '`printf 3443`' \
  '3443;printf 3443' \
  '$(printf ENROLLMENT_SIDE_EFFECT >&2)'; do
  assert_external_port_rejected ENROLLMENT_PORT "$payload"
  file_payload=$(printf 'ENROLLMENT_PORT=%s\n' "$payload")
  assert_file_rejected "existing .env $payload" "$file_payload"
done
newline_payload='3443
printf ENROLLMENT_SIDE_EFFECT'
assert_external_port_rejected ENROLLMENT_PORT "$newline_payload"
file_newline_payload=$(printf 'ENROLLMENT_PORT=%s\n' "$newline_payload")
assert_file_rejected 'existing .env newline payload' "$file_newline_payload"

for payload in 0 65536 999999; do
  assert_external_port_rejected ENROLLMENT_PORT "$payload"
done

for variable in WEB_PORT DNS_PORT; do
  assert_external_port_rejected "$variable" '$(printf 53)'
done

assert_main_rejects_before_side_effect() {
  variable=$1
  value=$2
  output=''
  if output=$(
    (
      PROXYCORE_HOME=/path/that/does/not/exist
      case "$variable" in
        WEB_PORT) WEB_PORT=$value ;;
        ENROLLMENT_PORT) ENROLLMENT_PORT=$value ;;
        DNS_PORT) DNS_PORT=$value ;;
        *) exit 1 ;;
      esac
      PROXYCORE_INSTALL_NO_MAIN=1 . "$ROOT/scripts/install.sh"
      need_cmd() { printf 'INSTALLER_SIDE_EFFECT\\n' >&2; exit 99; }
      sync_repo() { printf 'INSTALLER_SIDE_EFFECT\\n' >&2; exit 99; }
      log() { printf 'INSTALLER_SIDE_EFFECT\\n' >&2; exit 99; }
      main
    ) 2>&1
  ); then
    printf '%s was accepted before preflight\n' "$variable" >&2
    exit 1
  fi
  case "$output" in
    *INSTALLER_SIDE_EFFECT*)
      printf '%s reached a side effect before rejection\n' "$variable" >&2
      exit 1
      ;;
  esac
}

for variable in WEB_PORT ENROLLMENT_PORT DNS_PORT; do
  assert_main_rejects_before_side_effect "$variable" '$(printf ENROLLMENT_SIDE_EFFECT >&2)'
done

assert_file_preflight_rejects_before_side_effect() {
  input=$1
  output=''
  if output=$(printf '%s' "$input" | (
    unset WEB_PORT ENROLLMENT_PORT DNS_PORT POSTGRES_USER POSTGRES_DB
    PROXYCORE_INSTALL_NO_MAIN=1 . "$ROOT/scripts/install.sh"
    preflight_runtime_values -
    printf 'PREFLIGHT_SIDE_EFFECT\\n' >&2
  ) 2>&1); then
    printf 'malicious existing .env data was accepted\n' >&2
    exit 1
  fi
  case "$output" in
    *PREFLIGHT_SIDE_EFFECT*|*ENROLLMENT_SIDE_EFFECT*)
      printf 'existing .env data reached a side effect\n' >&2
      exit 1
      ;;
  esac
}
file_preflight_payload=$(printf 'ENROLLMENT_PORT=%s\n' '$(printf ENROLLMENT_SIDE_EFFECT >&2)')
assert_file_preflight_rejects_before_side_effect "$file_preflight_payload"

# Operator-facing docs distinguish the direct enrollment socket from Nginx.
contains "$ROOT/README.md" 'ENROLLMENT_PORT'
contains "$ROOT/README.md" 'not Nginx proxy ingress'
contains "$ROOT/docs/runbooks/primary-node.md" 'NODE trust entry/import is deferred to preview'
