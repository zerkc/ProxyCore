#!/bin/sh
# Tests scripts/convert-postgres.sh with a stubbed `docker` on PATH. No real
# Docker is used; the stub records argument lists (never environment values)
# and simulates volumes, containers and the proxycore-convert result.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
REAL_SCRIPT="$ROOT/scripts/convert-postgres.sh"

TEST_TMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/proxycore-convert-test.XXXXXX")
cleanup_test_tmp() {
  rm -rf "$TEST_TMP_DIR"
}
trap cleanup_test_tmp EXIT HUP INT TERM

# Run a copy inside a scratch tree so the script resolves its own ROOT there:
# no real .env or compose.yaml is ever read.
mkdir -p "$TEST_TMP_DIR/root/scripts"
cp "$REAL_SCRIPT" "$TEST_TMP_DIR/root/scripts/convert-postgres.sh"
SCRIPT="$TEST_TMP_DIR/root/scripts/convert-postgres.sh"

BIN="$TEST_TMP_DIR/bin"
mkdir -p "$BIN"
cat > "$BIN/docker" <<'STUB'
#!/bin/sh
# Stub state lives in $STUB_STATE: calls (log), files db/marker, flags.
printf '%s\n' "$*" >> "$STUB_STATE/calls"
case "$1" in
  info) exit 0 ;;
  volume)
    # docker volume inspect <name>
    [ -f "$STUB_STATE/legacy" ] && [ "$3" = "${STUB_PROJECT:-proxycore}_postgres-data" ] ;;
  ps)
    case "$*" in
      *com.docker.compose.service=api*) [ -f "$STUB_STATE/api_running" ] && echo abc123 || : ;;
      *com.docker.compose.service=worker*) [ -f "$STUB_STATE/worker_running" ] && echo def456 || : ;;
      *volume=*) [ -f "$STUB_STATE/volume_user" ] && echo ghi789 || : ;;
      *name=*) [ -f "$STUB_STATE/stale" ] && echo jkl012 || : ;;
    esac
    exit 0 ;;
  network | rm) exit 0 ;;
  run) exit 0 ;;
  exec) exit 0 ;;
  compose)
    shift
    case "$1" in
      version) exit 0 ;;
    esac
    case "$*" in
      *"--entrypoint test api -f /var/lib/proxycore-db/proxycore.db.converted.json"*) [ -f "$STUB_STATE/marker" ] ;;
      *"--entrypoint test api -f /var/lib/proxycore-db/proxycore.db"*) [ -f "$STUB_STATE/db" ] ;;
      *"--entrypoint sh api -c command -v proxycore-convert"*) [ ! -f "$STUB_STATE/no_converter" ] ;;
      *"--entrypoint proxycore-convert"*)
        # Record only presence of the URL variable, never its value.
        if [ -n "${PROXYCORE_CONVERT_PG_URL:-}" ]; then
          printf 'PGURL_SET\n' >> "$STUB_STATE/calls"
          case "$PROXYCORE_CONVERT_PG_URL" in
            *'sup3rS3cret%21x'*) printf 'PGURL_ENCODED\n' >> "$STUB_STATE/calls" ;;
          esac
        fi
        rc=${STUB_CONVERT_RC:-0}
        if [ "$rc" -eq 0 ]; then
          : > "$STUB_STATE/db"
          [ "${STUB_NO_MARKER:-0}" = 1 ] || : > "$STUB_STATE/marker"
        fi
        exit "$rc" ;;
      *) exit 0 ;;
    esac ;;
  *) exit 0 ;;
esac
STUB
chmod +x "$BIN/docker"

SECRET='sup3rS3cret!x'

# run_case <name> [env assignments handled by caller via exported STUB_*]
# Sets CASE_RC, CASE_OUT (stdout+stderr) and CASE_CALLS.
run_case() {
  STUB_STATE="$TEST_TMP_DIR/state"
  rm -rf "$STUB_STATE"
  mkdir -p "$STUB_STATE"
  : > "$STUB_STATE/calls"
  export STUB_STATE
  for flag in "$@"; do
    case "$flag" in
      legacy | api_running | worker_running | volume_user | stale | db | marker | no_converter) : > "$STUB_STATE/$flag" ;;
    esac
  done
  CASE_RC=0
  CASE_OUT=$(
    PATH="$BIN:$PATH" PROXYCORE_MASTER_KEY_BASE64=dummy-key \
      POSTGRES_USER=proxycore POSTGRES_DB=proxycore POSTGRES_PASSWORD="$SECRET" \
      sh "$SCRIPT" 2>&1
  ) || CASE_RC=$?
  CASE_CALLS=$(tr -d '\r' < "$STUB_STATE/calls")
}

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  printf -- '--- output ---\n%s\n--- calls ---\n%s\n' "$CASE_OUT" "$CASE_CALLS" >&2
  exit 1
}

assert_calls_have() {
  case "$CASE_CALLS" in
    *"$1"*) ;;
    *) fail "expected call containing: $1" ;;
  esac
}

assert_calls_lack() {
  case "$CASE_CALLS" in
    *"$1"*) fail "unexpected call containing: $1" ;;
  esac
}

assert_no_secret() {
  case "$CASE_OUT$CASE_CALLS" in
    *"$SECRET"* | *sup3rS3cret* | *dummy-key*) fail "credentials leaked into output or docker arguments" ;;
  esac
  case "$CASE_OUT$CASE_CALLS" in
    *postgres://*) fail "PostgreSQL URL leaked into output or docker arguments" ;;
  esac
}

assert_never_removes_volume() {
  case "$CASE_CALLS" in
    *"volume rm"* | *"volume prune"* | *"compose down"* | *"rm -fv"* | *"rm -v"*)
      fail "legacy volume must never be removed" ;;
  esac
}

# 1. No legacy volume: nothing to convert, exit 0, no container started.
run_case
[ "$CASE_RC" -eq 0 ] || fail "no legacy volume must exit 0"
assert_calls_lack "run -d"

# 2. Idempotent: database and marker already exist.
run_case legacy db marker
[ "$CASE_RC" -eq 0 ] || fail "already converted must exit 0"
assert_calls_lack "run -d"
assert_calls_lack "--entrypoint proxycore-convert"
case "$CASE_OUT" in *"Already converted"*) ;; *) fail "missing idempotent message" ;; esac

# 3. Database without marker is refused and never overwritten.
run_case legacy db
[ "$CASE_RC" -ne 0 ] || fail "database without marker must be refused"
assert_calls_lack "run -d"
assert_calls_lack "--entrypoint proxycore-convert"

# 4. Running api or worker is refused before any PostgreSQL starts.
for running in api_running worker_running; do
  run_case legacy "$running"
  [ "$CASE_RC" -ne 0 ] || fail "$running must be refused"
  assert_calls_lack "run -d"
  assert_calls_lack "network create"
  case "$CASE_OUT" in *"stop api and worker"*) ;; *) fail "missing stop instructions for $running" ;; esac
done

# 5. Another container on the legacy volume is refused.
run_case legacy volume_user
[ "$CASE_RC" -ne 0 ] || fail "container using the legacy volume must be refused"
assert_calls_lack "run -d"

# 6. Happy path.
run_case legacy
[ "$CASE_RC" -eq 0 ] || fail "happy path must exit 0"
assert_calls_have "network create --internal proxycore-legacy-pg-net"
assert_calls_have "run -d --rm --name proxycore-legacy-pg"
assert_calls_have "-v proxycore_postgres-data:/var/lib/postgresql/data"
assert_calls_have "--entrypoint proxycore-convert api --out /var/lib/proxycore-db/proxycore.db"
assert_calls_have "PGURL_SET"
assert_calls_have "PGURL_ENCODED"
assert_calls_have "rm -f proxycore-legacy-pg"
assert_calls_have "network rm proxycore-legacy-pg-net"
assert_calls_lack " -p "
assert_calls_lack "--publish"
assert_no_secret
assert_never_removes_volume

# 7. Converter failure: exit code is propagated, cleanup still runs.
for code in 1 2 3; do
  STUB_CONVERT_RC=$code
  export STUB_CONVERT_RC
  run_case legacy
  unset STUB_CONVERT_RC
  [ "$CASE_RC" -eq "$code" ] || fail "converter exit $code must propagate (got $CASE_RC)"
  assert_calls_have "rm -f proxycore-legacy-pg"
  assert_calls_have "network rm proxycore-legacy-pg-net"
  assert_no_secret
  assert_never_removes_volume
done

# 8. Converter success without a marker is not accepted.
STUB_NO_MARKER=1
export STUB_NO_MARKER
run_case legacy
unset STUB_NO_MARKER
[ "$CASE_RC" -ne 0 ] || fail "missing marker must fail the script"
assert_calls_have "rm -f proxycore-legacy-pg"

# 9. Missing master key is refused before any container starts.
STUB_STATE="$TEST_TMP_DIR/state"
rm -rf "$STUB_STATE"
mkdir -p "$STUB_STATE"
: > "$STUB_STATE/calls"
: > "$STUB_STATE/legacy"
export STUB_STATE
CASE_RC=0
CASE_OUT=$(env -u PROXYCORE_MASTER_KEY_BASE64 PATH="$BIN:$PATH" sh "$SCRIPT" 2>&1) || CASE_RC=$?
CASE_CALLS=$(tr -d '\r' < "$STUB_STATE/calls")
[ "$CASE_RC" -ne 0 ] || fail "missing master key must be refused"
assert_calls_lack "run -d"

# 9b. The api image has no proxycore-convert (an older release): refuse before
#     anything is stopped or started, with the build hint and exit 1.
run_case legacy no_converter
[ "$CASE_RC" -eq 1 ] || fail "missing converter binary must exit 1 (got $CASE_RC)"
assert_calls_lack "run -d"
assert_calls_lack "network create"
assert_calls_lack "--entrypoint proxycore-convert"
case "$CASE_OUT" in *"docker compose build api"*) ;; *) fail "missing-converter message must give the build hint" ;; esac
case "$CASE_OUT" in *proxycore-convert*) ;; *) fail "missing-converter message must name the binary" ;; esac
assert_no_secret

# 9c. COMPOSE_PROJECT_NAME is read from the install .env when the environment
#     does not set it. The .env values are used as data: the project selects
#     the volume, POSTGRES_PASSWORD reaches the converter URL, and the password
#     is not printed or logged.
printf 'COMPOSE_PROJECT_NAME=proxycore-t68\nPOSTGRES_PASSWORD=sup3rS3cret!x\n' > "$TEST_TMP_DIR/root/.env"
STUB_STATE="$TEST_TMP_DIR/state"
rm -rf "$STUB_STATE"
mkdir -p "$STUB_STATE"
: > "$STUB_STATE/calls"
: > "$STUB_STATE/legacy"
export STUB_STATE
CASE_RC=0
CASE_OUT=$(env -u COMPOSE_PROJECT_NAME -u POSTGRES_PASSWORD -u POSTGRES_USER -u POSTGRES_DB \
  PATH="$BIN:$PATH" PROXYCORE_MASTER_KEY_BASE64=dummy-key STUB_PROJECT=proxycore-t68 \
  sh "$SCRIPT" 2>&1) || CASE_RC=$?
CASE_CALLS=$(tr -d '\r' < "$STUB_STATE/calls")
[ "$CASE_RC" -eq 0 ] || fail ".env project must be honored and convert (got $CASE_RC)"
assert_calls_have "volume inspect proxycore-t68_postgres-data"
assert_calls_have "--entrypoint proxycore-convert api"
assert_calls_have "PGURL_ENCODED"
assert_no_secret

# 9d. The environment wins over .env.
CASE_RC=0
CASE_OUT=$(env -u POSTGRES_PASSWORD -u POSTGRES_USER -u POSTGRES_DB \
  PATH="$BIN:$PATH" PROXYCORE_MASTER_KEY_BASE64=dummy-key COMPOSE_PROJECT_NAME=proxycore STUB_PROJECT=proxycore \
  sh "$SCRIPT" 2>&1) || CASE_RC=$?
CASE_CALLS=$(tr -d '\r' < "$STUB_STATE/calls")
[ "$CASE_RC" -eq 0 ] || fail "environment project must be honored (got $CASE_RC)"
assert_calls_have "volume inspect proxycore_postgres-data"
rm -f "$TEST_TMP_DIR/root/.env"

# 10. Static guarantees on the script itself.
for forbidden in 'volume rm' 'down -v' 'rm -v' 'docker run .* -p ' '--publish'; do
  if grep -Eq -- "$forbidden" "$REAL_SCRIPT"; then
    printf 'script must not contain: %s\n' "$forbidden" >&2
    exit 1
  fi
done
if grep -Fq -- '. ./.env' "$REAL_SCRIPT" || grep -Fq -- '. "$ROOT/.env"' "$REAL_SCRIPT"; then
  printf 'script must not source .env\n' >&2
  exit 1
fi

printf 'convert-postgres tests passed\n'
