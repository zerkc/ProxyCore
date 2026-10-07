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
    unset WEB_PORT ENROLLMENT_PORT DNS_PORT
    PROXYCORE_INSTALL_NO_MAIN=1 . "$ROOT/scripts/install.sh"
    load_env_values -
    apply_env_file_values
    printf '%s|%s|%s\n' "$WEB_PORT" "$ENROLLMENT_PORT" "$DNS_PORT"
  )
}

assert_file_rejected() {
  label=$1
  input=$2
  output=''
  if output=$(printf '%s' "$input" | (
    unset WEB_PORT ENROLLMENT_PORT DNS_PORT
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
      # if-chain rather than case: bash 3.2 mis-parses case inside $( ( ... ) ).
      if [ "$variable" = WEB_PORT ]; then WEB_PORT=$value
      elif [ "$variable" = ENROLLMENT_PORT ]; then ENROLLMENT_PORT=$value
      elif [ "$variable" = DNS_PORT ]; then DNS_PORT=$value
      else exit 1
      fi
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

# Legacy POSTGRES_* keys in an existing .env are opaque unknown data now.
valid_env='WEB_PORT=3100
ENROLLMENT_PORT=9443
DNS_PORT=5353
POSTGRES_USER=bad user!
POSTGRES_DB=proxycore'
[ "$(run_loader "$valid_env")" = '3100|9443|5353' ] || {
  printf 'valid existing .env values were not loaded as data\n' >&2
  exit 1
}

external_env='WEB_PORT=3100
ENROLLMENT_PORT=9443
DNS_PORT=5353'
run_external_loader() {
  input=$1
  printf '%s' "$input" | (
    WEB_PORT=4100 ENROLLMENT_PORT=9444 DNS_PORT=5354
    PROXYCORE_INSTALL_NO_MAIN=1 . "$ROOT/scripts/install.sh"
    load_env_values -
    apply_env_file_values
    printf '%s|%s|%s\n' "$WEB_PORT" "$ENROLLMENT_PORT" "$DNS_PORT"
  )
}
[ "$(run_external_loader "$external_env")" = '4100|9444|5354' ] || {
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
      # if-chain rather than case: bash 3.2 mis-parses case inside $( ( ... ) ).
      if [ "$variable" = WEB_PORT ]; then WEB_PORT=$value
      elif [ "$variable" = ENROLLMENT_PORT ]; then ENROLLMENT_PORT=$value
      elif [ "$variable" = DNS_PORT ]; then DNS_PORT=$value
      else exit 1
      fi
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
    unset WEB_PORT ENROLLMENT_PORT DNS_PORT
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

# PostgreSQL is gone from the installer: no credentials, readiness wait or
# migrate step. The only PostgreSQL logic left is the legacy-volume refusal.
for forbidden in POSTGRES_USER POSTGRES_DB POSTGRES_PASSWORD wait_postgres 'run --rm migrate' 'build migrate'; do
  if grep -Fq -- "$forbidden" "$ROOT/scripts/install.sh" "$ROOT/scripts/install_env.sh"; then
    printf 'installer still references %s\n' "$forbidden" >&2
    exit 1
  fi
done

# Legacy detection: stub docker as a shell function inside a subshell.
run_legacy_check() {
  volume_present=$1
  db_present=$2
  (
    PROXYCORE_INSTALL_NO_MAIN=1 . "$ROOT/scripts/install.sh"
    PROXYCORE_HOME=/opt/proxycore
    docker() {
      case "$1 $2" in
        'volume inspect') [ "$volume_present" = 1 ] && [ "$3" = proxycore_postgres-data ] ;;
        'compose run') [ "$db_present" = 1 ] ;;
        *) return 1 ;;
      esac
    }
    refuse_unconverted_legacy_postgres
  ) 2>&1
}
output=$(run_legacy_check 1 0) && {
  printf 'legacy volume without SQLite database was accepted\n' >&2
  exit 1
}
case "$output" in
  *scripts/convert-postgres.sh*docs/runbooks/upgrade-from-postgres.md*) ;;
  *) printf 'legacy refusal does not point at the converter: %s\n' "$output" >&2; exit 1 ;;
esac
run_legacy_check 0 0 >/dev/null || { printf 'fresh install was refused\n' >&2; exit 1; }
run_legacy_check 1 1 >/dev/null || { printf 'converted install was refused\n' >&2; exit 1; }

# Operator-facing docs distinguish the direct enrollment socket from Nginx.
contains "$ROOT/README.md" 'ENROLLMENT_PORT'
contains "$ROOT/README.md" 'not Nginx proxy ingress'
contains "$ROOT/docs/runbooks/primary-node.md" 'NODE trust entry/import is deferred to preview'
