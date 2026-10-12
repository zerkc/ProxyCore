#!/bin/sh
# Tests scripts/migrate-legacy-postgres.sh with stubbed docker, pg_isready,
# psql, su-exec, chown and proxycore-convert on PATH. No real Docker or
# PostgreSQL is used. The stubs record argument lists (never environment
# values) so the tests can prove what was stopped, started and converted.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
REAL_SCRIPT="$ROOT/scripts/migrate-legacy-postgres.sh"

TEST_TMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/proxycore-migrate-test.XXXXXX")
cleanup_test_tmp() {
  rm -rf "$TEST_TMP_DIR"
}
trap cleanup_test_tmp EXIT HUP INT TERM

BIN="$TEST_TMP_DIR/bin"
mkdir -p "$BIN"

cat > "$BIN/docker" <<'STUB'
#!/bin/sh
# Stub state lives in $STUB_STATE: calls (log) and flag files.
printf 'docker %s\n' "$*" >> "$STUB_STATE/calls"
case "$1" in
  inspect)
    # Project label of the migrate container itself.
    printf 'proxycore-t66\n'
    exit 0 ;;
  volume)
    # docker volume inspect <name>
    [ -f "$STUB_STATE/legacy" ] && [ "$3" = proxycore-t66_postgres-data ] ;;
  ps)
    case "$*" in
      *com.docker.compose.service=api*) [ -f "$STUB_STATE/api_running" ] && echo abc123 || : ;;
      *com.docker.compose.service=worker*) [ -f "$STUB_STATE/worker_running" ] && echo def456 || : ;;
    esac
    exit 0 ;;
  stop)
    [ "${STUB_STOP_FAIL:-0}" = 1 ] && exit 1
    exit 0 ;;
  start) exit 0 ;;
  *) exit 0 ;;
esac
STUB
chmod +x "$BIN/docker"

cat > "$BIN/pg_isready" <<'STUB'
#!/bin/sh
printf 'pg_isready %s\n' "$*" >> "$STUB_STATE/calls"
[ -f "$STUB_STATE/pg_down" ] && exit 2
exit 0
STUB
chmod +x "$BIN/pg_isready"

cat > "$BIN/psql" <<'STUB'
#!/bin/sh
# Password arrives through PGPASSWORD only; it is never logged here.
printf 'psql %s\n' "$*" >> "$STUB_STATE/calls"
[ -f "$STUB_STATE/pg_auth_fail" ] && exit 2
exit 0
STUB
chmod +x "$BIN/psql"

cat > "$BIN/su-exec" <<'STUB'
#!/bin/sh
printf 'su-exec %s\n' "$1" >> "$STUB_STATE/calls"
shift
exec "$@"
STUB
chmod +x "$BIN/su-exec"

cat > "$BIN/chown" <<'STUB'
#!/bin/sh
printf 'chown %s\n' "$*" >> "$STUB_STATE/calls"
exit 0
STUB
chmod +x "$BIN/chown"

CONVERT_STUB="$BIN/proxycore-convert-stub"
cat > "$CONVERT_STUB" <<'STUB'
#!/bin/sh
# Records presence of the URL variable only, never its value.
printf 'convert %s\n' "$*" >> "$STUB_STATE/calls"
if [ -n "${PROXYCORE_CONVERT_PG_URL:-}" ]; then
  printf 'convert PGURL_SET\n' >> "$STUB_STATE/calls"
  case "$PROXYCORE_CONVERT_PG_URL" in
    *'sup3rS3cret%21x'*) printf 'convert PGURL_ENCODED\n' >> "$STUB_STATE/calls" ;;
  esac
fi
out=''
prev=''
for arg in "$@"; do
  [ "$prev" = --out ] && out=$arg
  prev=$arg
done
if [ "${STUB_CONVERT_SIGNAL:-0}" = 1 ]; then
  # Simulate an interrupt mid-conversion: partial temp output, then SIGTERM
  # to the migrate script (its parent), then keep running.
  : > "$out.tmp"
  kill -TERM "$PPID"
  sleep 5
  exit 0
fi
rc=${STUB_CONVERT_RC:-0}
if [ "$rc" -eq 0 ]; then
  : > "$out"
  [ "${STUB_NO_MARKER:-0}" = 1 ] || : > "$out.converted.json"
  printf 'Converted 2 tables, 3 rows.\n'
else
  printf 'convert: conversion failed\n' >&2
fi
exit "$rc"
STUB
chmod +x "$CONVERT_STUB"

SECRET='sup3rS3cret!x'

# run_case <flags...> : fresh state, data dir and PATH stubs. Flags:
#   legacy api_running worker_running pg_down pg_auth_fail db marker
run_case() {
  STUB_STATE="$TEST_TMP_DIR/state"
  rm -rf "$STUB_STATE"
  mkdir -p "$STUB_STATE/data"
  : > "$STUB_STATE/calls"
  export STUB_STATE
  for flag in "$@"; do
    case "$flag" in
      legacy | api_running | worker_running | pg_down | pg_auth_fail) : > "$STUB_STATE/$flag" ;;
      db) : > "$STUB_STATE/data/proxycore.db" ;;
      marker) : > "$STUB_STATE/data/proxycore.db.converted.json" ;;
    esac
  done
  CASE_RC=0
  CASE_OUT=$(
    PATH="$BIN:$PATH" \
      HOSTNAME=0123456789ab \
      PROXYCORE_MIGRATE_DATA_DIR="$STUB_STATE/data" \
      PROXYCORE_CONVERT_BIN="$CONVERT_STUB" \
      PROXYCORE_MASTER_KEY_BASE64="${CASE_KEY-dummy-key}" \
      POSTGRES_USER=proxycore POSTGRES_DB=proxycore POSTGRES_PASSWORD="$SECRET" \
      sh "$REAL_SCRIPT" 2>&1
  ) || CASE_RC=$?
  CASE_CALLS=$(tr -d '\r' < "$STUB_STATE/calls")
  unset CASE_KEY
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

assert_no_service_stopped() {
  assert_calls_lack "docker stop"
  assert_calls_lack "docker start"
  assert_calls_lack "convert "
}

assert_no_secret() {
  case "$CASE_OUT$CASE_CALLS" in
    *"$SECRET"* | *sup3rS3cret* | *dummy-key*) fail "credentials leaked into output or logged arguments" ;;
  esac
  case "$CASE_OUT$CASE_CALLS" in
    *postgres://*) fail "PostgreSQL URL leaked into output or logged arguments" ;;
  esac
}

# 1. No legacy volume: nothing to convert, exit 0, nothing stopped.
run_case
[ "$CASE_RC" -eq 0 ] || fail "no legacy volume must exit 0"
assert_no_service_stopped
case "$CASE_OUT" in *"nothing to convert"*) ;; *) fail "missing nothing-to-convert message" ;; esac

# 2. Idempotent: database and marker already exist.
run_case legacy api_running db marker
[ "$CASE_RC" -eq 0 ] || fail "already converted must exit 0"
assert_no_service_stopped
case "$CASE_OUT" in *"Already converted"*) ;; *) fail "missing idempotent message" ;; esac

# 3. Database without marker is refused and never touched.
run_case legacy api_running db
[ "$CASE_RC" -ne 0 ] || fail "database without marker must be refused"
assert_no_service_stopped
[ -f "$STUB_STATE/data/proxycore.db" ] || fail "refused database must be kept"

# 4. Marker without database is refused as well.
run_case legacy api_running marker
[ "$CASE_RC" -ne 0 ] || fail "marker without database must be refused"
assert_no_service_stopped

# 5. Missing master key is refused before anything is stopped or contacted.
CASE_KEY=''
run_case legacy api_running
[ "$CASE_RC" -ne 0 ] || fail "missing master key must be refused"
assert_no_service_stopped
assert_calls_lack "psql"
assert_calls_lack "pg_isready"
case "$CASE_OUT" in *PROXYCORE_MASTER_KEY_BASE64*) ;; *) fail "missing key message must name the variable" ;; esac

# 6. Legacy PostgreSQL unreachable: fail closed before anything is stopped.
run_case legacy api_running pg_down
[ "$CASE_RC" -ne 0 ] || fail "unreachable PostgreSQL must be refused"
assert_no_service_stopped
case "$CASE_OUT" in *convert-postgres.sh*) ;; *) fail "unreachable message must point to the manual converter" ;; esac

# 7. Legacy PostgreSQL rejects the credentials: fail closed before stopping.
run_case legacy api_running pg_auth_fail
[ "$CASE_RC" -ne 0 ] || fail "rejected credentials must be refused"
assert_no_service_stopped
assert_no_secret

# 8. Happy path: api and worker stopped once, converter run as uid 10001,
#    database and marker published with the runtime owner, exit 0.
run_case legacy api_running worker_running
[ "$CASE_RC" -eq 0 ] || fail "happy path must exit 0"
assert_calls_have "docker stop abc123 def456"
assert_calls_lack "docker start"
assert_calls_have "su-exec 10001"
assert_calls_have "convert --out $STUB_STATE/data/proxycore.db"
assert_calls_have "convert PGURL_SET"
assert_calls_have "convert PGURL_ENCODED"
assert_calls_have "chown 10001:10001 $STUB_STATE/data/proxycore.db"
assert_calls_have "chown 10001:10001 $STUB_STATE/data/proxycore.db.converted.json"
[ -f "$STUB_STATE/data/proxycore.db" ] || fail "database must exist after success"
[ -f "$STUB_STATE/data/proxycore.db.converted.json" ] || fail "marker must exist after success"
case "$CASE_OUT" in *"Converted 2 tables"*) ;; *) fail "converter summary must be printed" ;; esac
assert_no_secret

# 9. Success with no services running: nothing stopped, converter still runs.
run_case legacy
[ "$CASE_RC" -eq 0 ] || fail "success without running services must exit 0"
assert_calls_lack "docker stop"
assert_calls_have "convert --out"

# 10. Converter failure: exit code propagates, api and worker restarted,
#     no partial output left behind, no secrets printed.
for code in 1 2 3; do
  STUB_CONVERT_RC=$code
  export STUB_CONVERT_RC
  run_case legacy api_running worker_running
  unset STUB_CONVERT_RC
  [ "$CASE_RC" -eq "$code" ] || fail "converter exit $code must propagate (got $CASE_RC)"
  assert_calls_have "docker stop abc123 def456"
  assert_calls_have "docker start abc123 def456"
  [ ! -e "$STUB_STATE/data/proxycore.db" ] || fail "failed conversion must not leave a database"
  [ ! -e "$STUB_STATE/data/proxycore.db.converted.json" ] || fail "failed conversion must not leave a marker"
  assert_no_secret
done

# 11. Converter success without a marker is not accepted: services restarted,
#     the unmarked database removed.
STUB_NO_MARKER=1
export STUB_NO_MARKER
run_case legacy api_running
unset STUB_NO_MARKER
[ "$CASE_RC" -ne 0 ] || fail "missing marker must fail the script"
assert_calls_have "docker start abc123"
[ ! -e "$STUB_STATE/data/proxycore.db" ] || fail "unmarked database must be removed"

# 12. Interrupt mid-conversion (SIGTERM to the script): the converter is
#     stopped, partial temp output removed, services restarted, exit 143.
STUB_CONVERT_SIGNAL=1
export STUB_CONVERT_SIGNAL
run_case legacy api_running worker_running
unset STUB_CONVERT_SIGNAL
[ "$CASE_RC" -eq 143 ] || fail "interrupt must exit 143 (got $CASE_RC)"
assert_calls_have "docker start abc123 def456"
[ ! -e "$STUB_STATE/data/proxycore.db.tmp" ] || fail "interrupt must remove partial temp output"
[ ! -e "$STUB_STATE/data/proxycore.db" ] || fail "interrupt must not publish a database"

# 13. Failed stop: nothing converted, services already stopped are restarted.
STUB_STOP_FAIL=1
export STUB_STOP_FAIL
run_case legacy api_running worker_running
unset STUB_STOP_FAIL
[ "$CASE_RC" -ne 0 ] || fail "failed stop must fail the script"
assert_calls_lack "convert "
assert_calls_have "docker start abc123 def456"

# 14. Static guarantees on the script itself.
for forbidden in 'volume rm' 'volume prune' 'compose down' 'rm -v' 'docker rm' '--pg-url' '--publish'; do
  if grep -Fq -- "$forbidden" "$REAL_SCRIPT"; then
    printf 'script must not contain: %s\n' "$forbidden" >&2
    exit 1
  fi
done
if grep -Eq '(^|[[:space:]])(\.|source)[[:space:]].*\.env' "$REAL_SCRIPT"; then
  printf 'script must not source .env\n' >&2
  exit 1
fi

printf 'migrate-legacy-postgres tests passed\n'
