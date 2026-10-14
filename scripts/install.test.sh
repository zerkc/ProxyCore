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

# The legacy volume name follows the compose project: environment first, then
# the install .env (read as data, never sourced or printed), then proxycore.
# The docker stub reports only $project_volume as present.
project_legacy_output=''
project_legacy_refused() {
  project_volume=$1
  project_env=$2
  project_env_content=$3
  project_home=$(mktemp -d "${TMPDIR:-/tmp}/proxycore-install-test.XXXXXX")
  if [ -n "$project_env_content" ]; then
    printf '%s\n' "$project_env_content" > "$project_home/.env"
  fi
  if project_legacy_output=$( (
    unset COMPOSE_PROJECT_NAME
    if [ -n "$project_env" ]; then
      COMPOSE_PROJECT_NAME=$project_env
      export COMPOSE_PROJECT_NAME
    fi
    PROXYCORE_INSTALL_NO_MAIN=1 . "$ROOT/scripts/install.sh"
    PROXYCORE_HOME=$project_home
    docker() {
      # if-chain rather than case: bash 3.2 mis-parses case inside $( ( ... ) ).
      if [ "$1 $2" = 'volume inspect' ]; then
        [ "$3" = "$project_volume" ]
      else
        return 1
      fi
    }
    refuse_unconverted_legacy_postgres
  ) 2>&1 ); then
    rm -rf "$project_home"
    return 1
  fi
  rm -rf "$project_home"
  return 0
}

# .env selects a non-default project: the refusal must name its volume.
project_env_t69='COMPOSE_PROJECT_NAME=proxycore-t69
POSTGRES_PASSWORD=sup3rS3cret-t69'
project_legacy_refused proxycore-t69_postgres-data '' "$project_env_t69" \
  || { printf '.env project did not trigger the legacy refusal\n' >&2; exit 1; }
case "$project_legacy_output" in
  *proxycore-t69_postgres-data*) ;;
  *) printf 'legacy refusal does not name the .env project volume: %s\n' "$project_legacy_output" >&2; exit 1 ;;
esac
case "$project_legacy_output" in
  *sup3rS3cret-t69*) printf 'legacy refusal printed a .env value\n' >&2; exit 1 ;;
esac

# The default volume is not the .env project's volume: no refusal.
project_legacy_refused proxycore_postgres-data '' "$project_env_t69" \
  && { printf 'default volume refused although .env selects another project\n' >&2; exit 1; }

# The environment wins over .env.
project_legacy_refused proxycore-env_postgres-data proxycore-env "$project_env_t69" \
  || { printf 'environment project did not win over .env\n' >&2; exit 1; }

# Without COMPOSE_PROJECT_NAME anywhere, the default project still refuses.
project_legacy_refused proxycore_postgres-data '' 'POSTGRES_PASSWORD=unused-secret' \
  || { printf 'default project did not trigger the legacy refusal\n' >&2; exit 1; }
project_legacy_refused proxycore_postgres-data '' '' \
  || { printf 'default project without .env did not trigger the legacy refusal\n' >&2; exit 1; }

# Fresh install: no .env yet and no legacy volume is accepted without error.
project_legacy_refused none '' '' \
  && { printf 'fresh install with no .env was refused\n' >&2; exit 1; }

# Operator-facing docs distinguish the direct enrollment socket from Nginx.
contains "$ROOT/README.md" 'ENROLLMENT_PORT'
contains "$ROOT/README.md" 'not Nginx proxy ingress'
contains "$ROOT/docs/runbooks/primary-node.md" 'NODE trust entry/import is deferred to preview'

# Sequential build: every buildable service is built on its own, in a fixed
# order, then `up` runs once without --build. The docker stub records calls.
CALLS_DIR=$(mktemp -d "${TMPDIR:-/tmp}/proxycore-install-calls.XXXXXX")
trap 'rm -rf "$CALLS_DIR"' EXIT HUP INT TERM

# run_deploy <skip_build> [failing_service] [attestations: unset|<value>] [mode]
# Prints the recorded docker calls and installer output; returns the exit code.
# mode is build by default so the sequential-build tests below stay unchanged;
# the pull-mode tests pass pull explicitly.
run_deploy() {
  deploy_skip=$1
  deploy_fail=${2:-}
  deploy_att=${3:-unset}
  deploy_mode=${4:-build}
  : > "$CALLS_DIR/calls"
  (
    PROXYCORE_INSTALL_NO_MAIN=1 . "$ROOT/scripts/install.sh"
    INSTALL_MODE=$deploy_mode
    SKIP_BUILD=$deploy_skip
    PROXYCORE_SKIP_MEMORY_CHECK=1
    unset BUILDX_NO_DEFAULT_ATTESTATIONS
    if [ "$deploy_att" != unset ]; then
      BUILDX_NO_DEFAULT_ATTESTATIONS=$deploy_att
      export BUILDX_NO_DEFAULT_ATTESTATIONS
    fi
    docker() {
      printf 'docker %s att=%s\n' "$*" "${BUILDX_NO_DEFAULT_ATTESTATIONS:-unset}" >> "$CALLS_DIR/calls"
      if [ "$1 $2" = 'compose build' ] && [ "$3" = "$deploy_fail" ]; then
        return 1
      fi
      if [ "$1 $2" = 'compose pull' ] && [ "$deploy_fail" = pull ]; then
        return 1
      fi
      return 0
    }
    deploy_stack
  ) 2>&1
}

expected_services='coredns nginx updater-bootstrap updater api worker'

output=$(run_deploy 0) || { printf 'sequential deploy failed: %s\n' "$output" >&2; exit 1; }
built=$(sed -n 's/^docker compose build \([^ ]*\) att=.*/\1/p' "$CALLS_DIR/calls" | tr '\n' ' ')
[ "$built" = "$expected_services " ] || {
  printf 'unexpected build order: %s\n' "$built" >&2
  exit 1
}
if grep -Fq -- '--build' "$CALLS_DIR/calls"; then
  printf 'up must not use --build\n' >&2
  exit 1
fi
[ "$(grep -c '^docker compose up ' "$CALLS_DIR/calls")" -eq 1 ] || {
  printf 'up must run exactly once\n' >&2
  exit 1
}
grep -Fq 'docker compose up -d --remove-orphans att=' "$CALLS_DIR/calls" || {
  printf 'unexpected up invocation\n' >&2
  exit 1
}
[ "$(sed -n '$p' "$CALLS_DIR/calls" | cut -c1-17)" = 'docker compose up' ] || {
  printf 'up must run after every build\n' >&2
  exit 1
}
case "$output" in
  *'[1/6] building coredns'*'[6/6] building worker'*) ;;
  *) printf 'missing progress lines: %s\n' "$output" >&2; exit 1 ;;
esac
case "$output" in
  *'built coredns in '*s*) ;;
  *) printf 'missing elapsed time line: %s\n' "$output" >&2; exit 1 ;;
esac

# Attestations default to off, and a preset value is never overridden.
grep -Fq 'compose build api att=1' "$CALLS_DIR/calls" || {
  printf 'attestations were not disabled by default\n' >&2
  exit 1
}
run_deploy 0 '' 0 >/dev/null
grep -Fq 'compose build api att=0' "$CALLS_DIR/calls" || {
  printf 'preset BUILDX_NO_DEFAULT_ATTESTATIONS was overridden\n' >&2
  exit 1
}

# SKIP_BUILD=1 keeps the old behavior: no build, one up.
run_deploy 1 >/dev/null || { printf 'SKIP_BUILD deploy failed\n' >&2; exit 1; }
if grep -Fq 'compose build' "$CALLS_DIR/calls"; then
  printf 'SKIP_BUILD=1 must not build\n' >&2
  exit 1
fi
[ "$(grep -c '^docker compose up -d --remove-orphans' "$CALLS_DIR/calls")" -eq 1 ] || {
  printf 'SKIP_BUILD=1 must run up once\n' >&2
  exit 1
}

# A failed build stops before up, names the service, and exits non-zero.
if output=$(run_deploy 0 updater); then
  printf 'failed build did not stop the install\n' >&2
  exit 1
fi
if grep -Fq 'compose up' "$CALLS_DIR/calls"; then
  printf 'up ran after a failed build\n' >&2
  exit 1
fi
if grep -Fq 'compose build api' "$CALLS_DIR/calls"; then
  printf 'builds continued after a failure\n' >&2
  exit 1
fi
case "$output" in
  *'updater'*'docker compose build updater'*) ;;
  *) printf 'failure message does not name the service/retry: %s\n' "$output" >&2; exit 1 ;;
esac

# The ordered list must match the services that carry a build section in
# compose.yaml (profile-gated `migrate` excluded; `control` reuses worker's image).
compose_buildable=$(awk '
  /^services:/ { in_services = 1; next }
  /^[a-z]/ { in_services = 0 }
  in_services && /^  [a-z][a-z0-9-]*:[[:space:]]*$/ { svc = $1; sub(":", "", svc); next }
  in_services && /^    build:/ { print svc }
' "$ROOT/compose.yaml" | grep -vx migrate | sort | tr '\n' ' ')
listed=$(printf '%s\n' $expected_services | sort | tr '\n' ' ')
[ "$compose_buildable" = "$listed" ] || {
  printf 'BUILD_SERVICES drifted from compose.yaml: compose=[%s] list=[%s]\n' "$compose_buildable" "$listed" >&2
  exit 1
}
(
  PROXYCORE_INSTALL_NO_MAIN=1 . "$ROOT/scripts/install.sh"
  [ "$BUILD_SERVICES" = "$expected_services" ]
) || { printf 'install.sh BUILD_SERVICES differs from the expected order\n' >&2; exit 1; }

# Low-memory guard: warns on a small host without swap, never fails, and can
# be silenced. /proc/meminfo is replaced through PROXYCORE_MEMINFO_PATH.
run_memory_check() {
  mem_total=$1
  mem_swap=$2
  mem_skip=${3:-0}
  mem_file="$CALLS_DIR/meminfo"
  printf 'MemTotal:       %s kB\nMemFree: 1 kB\nSwapTotal:      %s kB\n' "$mem_total" "$mem_swap" > "$mem_file"
  (
    PROXYCORE_INSTALL_NO_MAIN=1 . "$ROOT/scripts/install.sh"
    PROXYCORE_MEMINFO_PATH=$mem_file
    PROXYCORE_SKIP_MEMORY_CHECK=$mem_skip
    check_build_memory
  ) 2>&1
}
output=$(run_memory_check 920000 0) || { printf 'memory check must never fail\n' >&2; exit 1; }
case "$output" in
  *swap*) ;;
  *) printf 'low-memory host did not warn: %s\n' "$output" >&2; exit 1 ;;
esac
output=$(run_memory_check 920000 0 1)
[ -z "$output" ] || { printf 'PROXYCORE_SKIP_MEMORY_CHECK=1 did not silence the check: %s\n' "$output" >&2; exit 1; }
output=$(run_memory_check 920000 2097152)
[ -z "$output" ] || { printf 'host with enough swap warned: %s\n' "$output" >&2; exit 1; }
output=$(run_memory_check 8000000 0)
[ -z "$output" ] || { printf 'host with enough RAM warned: %s\n' "$output" >&2; exit 1; }
output=$(
  PROXYCORE_INSTALL_NO_MAIN=1 . "$ROOT/scripts/install.sh"
  PROXYCORE_MEMINFO_PATH=/path/that/does/not/exist
  PROXYCORE_SKIP_MEMORY_CHECK=0
  check_build_memory
) 2>&1
[ -z "$output" ] || { printf 'missing meminfo must be silent: %s\n' "$output" >&2; exit 1; }

# Default mode is pull: the installer reads PROXYCORE_INSTALL_MODE, defaulting
# to pull, and a fresh source of install.sh without it must not build.
default_mode=$(
  unset PROXYCORE_INSTALL_MODE
  PROXYCORE_INSTALL_NO_MAIN=1 . "$ROOT/scripts/install.sh"
  printf '%s' "$INSTALL_MODE"
)
[ "$default_mode" = pull ] || { printf 'default install mode is %s, want pull\n' "$default_mode" >&2; exit 1; }
build_mode=$(
  PROXYCORE_INSTALL_MODE=build
  PROXYCORE_INSTALL_NO_MAIN=1 . "$ROOT/scripts/install.sh"
  printf '%s' "$INSTALL_MODE"
)
[ "$build_mode" = build ] || { printf 'PROXYCORE_INSTALL_MODE=build was ignored\n' >&2; exit 1; }

# Pull mode: one pull of every service, then one up, and never a build.
output=$(run_deploy 0 '' unset pull) || { printf 'pull deploy failed: %s\n' "$output" >&2; exit 1; }
if grep -Fq 'compose build' "$CALLS_DIR/calls"; then
  printf 'pull mode must not build\n' >&2
  exit 1
fi
[ "$(grep -c '^docker compose pull ' "$CALLS_DIR/calls")" -eq 1 ] || {
  printf 'pull mode must pull exactly once\n' >&2
  exit 1
}
for svc in api worker control nginx coredns updater updater-bootstrap; do
  grep '^docker compose pull ' "$CALLS_DIR/calls" | grep -Fq -- " $svc" || {
    printf 'pull mode did not pull %s\n' "$svc" >&2
    exit 1
  }
done
[ "$(sed -n '$p' "$CALLS_DIR/calls" | cut -c1-17)" = 'docker compose up' ] || {
  printf 'up must run after the pull\n' >&2
  exit 1
}
case "$output" in
  *'low memory'*) printf 'pull mode must not show the build memory warning\n' >&2; exit 1 ;;
esac

# A failed pull stops before up.
if output=$(run_deploy 0 pull unset pull); then
  printf 'failed pull did not stop the install\n' >&2
  exit 1
fi
if grep -Fq 'compose up' "$CALLS_DIR/calls"; then
  printf 'up ran after a failed pull\n' >&2
  exit 1
fi

# SKIP_BUILD=1 skips the pull as well.
run_deploy 1 '' unset pull >/dev/null || { printf 'SKIP_BUILD pull deploy failed\n' >&2; exit 1; }
if grep -Eq 'compose (pull|build)' "$CALLS_DIR/calls"; then
  printf 'SKIP_BUILD=1 must not pull or build\n' >&2
  exit 1
fi

# Mode validation.
output=$(
  (
    PROXYCORE_INSTALL_MODE=bogus
    PROXYCORE_INSTALL_NO_MAIN=1 . "$ROOT/scripts/install.sh"
    validate_install_mode
  ) 2>&1
) && { printf 'invalid PROXYCORE_INSTALL_MODE was accepted\n' >&2; exit 1; }

# Architecture handling. PROXYCORE_UNAME_M stands in for `uname -m`.
run_arch() {
  arch_value=$1
  arch_allow=${2:-0}
  (
    PROXYCORE_INSTALL_NO_MAIN=1 . "$ROOT/scripts/install.sh"
    PROXYCORE_UNAME_M=$arch_value
    PROXYCORE_ALLOW_UNSUPPORTED_ARCH=$arch_allow
    check_arch
  ) 2>&1
}
for supported in x86_64 amd64 aarch64 arm64; do
  output=$(run_arch "$supported") || { printf '%s was refused: %s\n' "$supported" "$output" >&2; exit 1; }
  [ -z "$output" ] || { printf '%s printed output: %s\n' "$supported" "$output" >&2; exit 1; }
done
for arm32 in armv7l armv6l; do
  output=$(run_arch "$arm32") && { printf '%s was accepted\n' "$arm32" >&2; exit 1; }
  case "$output" in
    *'bun-based worker'*'64-bit OS'*) ;;
    *) printf '%s refusal is not actionable: %s\n' "$arm32" "$output" >&2; exit 1 ;;
  esac
  output=$(run_arch "$arm32" 1) || { printf '%s override was refused: %s\n' "$arm32" "$output" >&2; exit 1; }
  case "$output" in
    *warning*) ;;
    *) printf '%s override did not warn: %s\n' "$arm32" "$output" >&2; exit 1 ;;
  esac
done
for x86_32 in i386 i686; do
  output=$(run_arch "$x86_32") && { printf '%s was accepted\n' "$x86_32" >&2; exit 1; }
  output=$(run_arch "$x86_32" 1) && { printf '%s override must not apply to 32-bit x86\n' "$x86_32" >&2; exit 1; }
done

# Every compose image defaults to GHCR (never a :local tag) and stays overridable.
contains "$ROOT/compose.yaml" 'PROXYCORE_IMAGE_REPO:-ghcr.io/zerkc'
contains "$ROOT/compose.yaml" 'PROXYCORE_VERSION:-latest'
if grep -Fq -- ':local' "$ROOT/compose.yaml"; then
  printf 'compose.yaml still references a :local image\n' >&2
  exit 1
fi
if grep -Fq -- 'pull_policy: never' "$ROOT/compose.yaml"; then
  printf 'compose.yaml must not forbid pulling\n' >&2
  exit 1
fi
