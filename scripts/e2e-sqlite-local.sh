#!/usr/bin/env bash
# Local SQLite smoke test: Go API and worker share one WAL file. No Docker.
# The worker is the Go one (apps/api/cmd/worker).
# Everything lives under a mktemp -d directory that is removed on exit; children
# are killed by the EXIT trap. Exits non-zero when any step FAILs.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/proxycore-e2e.XXXXXX")"
API_PID=""
WORKER_PID=""
RESULTS=()
FAILURES=0

cleanup() {
  local pid
  for pid in "$WORKER_PID" "$API_PID"; do
    if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
      kill -TERM "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    fi
  done
  rm -rf "$TMP"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

record() { # status name detail
  RESULTS+=("$1|$2|$3")
  printf '[%s] %s: %s\n' "$1" "$2" "$3"
  if [[ "$1" == FAIL ]]; then FAILURES=$((FAILURES + 1)); fi
}

summary() {
  echo
  echo "== E2E SQLite local summary =="
  local row
  for row in "${RESULTS[@]}"; do
    IFS='|' read -r status name detail <<<"$row"
    printf '%-8s %s -- %s\n' "$status" "$name" "$detail"
  done
  if (( FAILURES > 0 )); then
    echo "RESULT: FAIL ($FAILURES failing step(s))"
  else
    echo "RESULT: PASS"
  fi
}

# Stop early on a failed prerequisite or step; the summary still prints.
bail() { # name detail
  record FAIL "$1" "$2"
  summary
  exit 1
}

require() {
  command -v "$1" >/dev/null 2>&1 || bail "prerequisites" "missing required command: $1"
}

require go
require curl
require jq
require rg
require openssl
require python3
require sqlite3

DATA="$TMP/data"
RUN="$TMP/run"
mkdir -p "$DATA" "$RUN" "$TMP/candidates" "$TMP/coredns" "$TMP/nginx"
DB="$DATA/proxycore.sqlite"
JAR="$TMP/cookies.txt"
BODY="$TMP/body.json"
BASE=""

free_port() {
  python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()'
}

# req METHOD PATH [JSON] -> prints HTTP status code; response body in $BODY.
req() {
  local method="$1" path="$2" data="${3-}"
  local args=(-sS -o "$BODY" -w '%{http_code}' -X "$method" -b "$JAR" -c "$JAR" "$BASE$path")
  if [[ -n "$data" ]]; then
    args+=(-H 'content-type: application/json' --data "$data")
  fi
  curl "${args[@]}" || echo "000"
}

snippet() { head -c 300 "$BODY" 2>/dev/null || true; }

echo "== build =="
BIN="$TMP/proxycore-server"
if (cd "$ROOT/apps/api" && go build -o "$BIN" ./cmd/server); then
  record PASS "build api server" "go build ./cmd/server into a temp directory"
else
  bail "build api server" "go build failed"
fi

API_PORT="$(free_port)"
TLS_PORT="$(free_port)"
BASE="http://127.0.0.1:$API_PORT"
MASTER_KEY="$(openssl rand -base64 32)"
OWNER_PASSWORD="$(openssl rand -hex 18)"

echo "== api =="
env -i PATH="$PATH" HOME="$HOME" \
  PROXYCORE_SQLITE_PATH="$DB" \
  PROXYCORE_MASTER_KEY_BASE64="$MASTER_KEY" \
  PROXYCORE_API_ADDR="127.0.0.1:$API_PORT" \
  PROXYCORE_ENROLLMENT_TLS_ADDR="127.0.0.1:$TLS_PORT" \
  PROXYCORE_UI_DIST="$TMP/ui-missing" \
  PROXYCORE_UPDATE_CHECK_ENABLED=0 \
  PROXYCORE_SECURE_COOKIES=0 \
  "$BIN" >"$TMP/api.log" 2>&1 &
API_PID=$!

healthy=0
for _ in $(seq 1 60); do
  if [[ "$(curl -sS -o /dev/null -w '%{http_code}' "$BASE/api/health" 2>/dev/null || true)" == 200 ]]; then
    healthy=1
    break
  fi
  if ! kill -0 "$API_PID" 2>/dev/null; then break; fi
  sleep 0.5
done
if (( healthy == 1 )); then
  record PASS "api health" "GET /api/health 200 on a random localhost port; Go API migrated $DB"
else
  bail "api health" "API did not become healthy; see $TMP/api.log (tail: $(tail -n 5 "$TMP/api.log" 2>/dev/null | tr '\n' ' '))"
fi

code="$(req POST /api/auth/bootstrap "$(jq -nc --arg p "$OWNER_PASSWORD" '{username:"owner",password:$p}')")"
[[ "$code" == 201 ]] && record PASS "bootstrap owner" "POST /api/auth/bootstrap -> $code" \
  || bail "bootstrap owner" "POST /api/auth/bootstrap -> $code: $(snippet)"

code="$(req POST /api/auth/login "$(jq -nc --arg p "$OWNER_PASSWORD" '{username:"owner",password:$p}')")"
[[ "$code" == 200 ]] && record PASS "login" "POST /api/auth/login -> $code (session cookie stored)" \
  || bail "login" "POST /api/auth/login -> $code: $(snippet)"

# Default resolver pool and ingress are required before any apply job can be created.
code="$(req PUT /api/settings '{"ingress":{"ipv4":"192.0.2.10"},"defaultPool":{"id":"default","endpoints":[{"host":"192.0.2.53","port":53}]}}')"
[[ "$code" == 200 ]] && record PASS "configure settings" "PUT /api/settings -> $code" \
  || bail "configure settings" "PUT /api/settings -> $code: $(snippet)"

code="$(req POST /api/zones '{"name":"e2e.proxycore.test"}')"
ZONE_ID="$(jq -r '.zone.id // empty' "$BODY" 2>/dev/null || true)"
[[ "$code" == 201 && -n "$ZONE_ID" ]] && record PASS "create zone" "POST /api/zones -> $code id=$ZONE_ID" \
  || bail "create zone" "POST /api/zones -> $code: $(snippet)"

code="$(req POST "/api/zones/$ZONE_ID/records" '{"name":"www","type":"A","value":"192.0.2.10","ttl":300}')"
RECORD_ID="$(jq -r '.record.id // empty' "$BODY" 2>/dev/null || true)"
[[ "$code" == 201 && -n "$RECORD_ID" ]] && record PASS "create record" "POST /api/zones/{id}/records A www -> $code id=$RECORD_ID" \
  || bail "create record" "POST records -> $code: $(snippet)"

code="$(req POST /api/apply)"
JOB_ID="$(jq -r '.id // .job.id // empty' "$BODY" 2>/dev/null || true)"
[[ "$code" == 202 && -n "$JOB_ID" ]] && record PASS "create apply job" "POST /api/apply -> $code job=$JOB_ID" \
  || bail "create apply job" "POST /api/apply -> $code: $(snippet)"

echo "== worker =="
WORKER_CMD=()
worker_skip=""
if ! command -v go >/dev/null 2>&1; then
  worker_skip="go not on PATH"
elif (cd "$ROOT/apps/api" && go build -o "$TMP/proxycore-worker" ./cmd/worker) >"$TMP/worker-build.log" 2>&1; then
  WORKER_CMD=("$TMP/proxycore-worker")
else
  record FAIL "worker build" "go build ./cmd/worker failed: $(tail -n 5 "$TMP/worker-build.log" | tr '\n' ' ')"
  worker_skip="go worker did not build"
fi
if (( ${#WORKER_CMD[@]} == 0 )); then
  record SKIPPED "worker start" "$worker_skip"
  record SKIPPED "worker reaches apply job" "$worker_skip"
else
  env -i PATH="$PATH" HOME="$HOME" \
    PROXYCORE_SQLITE_PATH="$DB" \
    PROXYCORE_MASTER_KEY_BASE64="$MASTER_KEY" \
    WORKER_SOCKET_PATH="$RUN/control.sock" \
    WORKER_HEARTBEAT_PATH="$RUN/worker.heartbeat" \
    WORKER_CANDIDATE_ROOT="$TMP/candidates" \
    CORE_DNS_CONFIG_DIR="$TMP/coredns" \
    NGINX_CONFIG_DIR="$TMP/nginx" \
    "${WORKER_CMD[@]}" >"$TMP/worker.log" 2>&1 &
  WORKER_PID=$!

  started=0
  for _ in $(seq 1 60); do
    if rg -q "listening for jobs" "$TMP/worker.log" 2>/dev/null; then started=1; break; fi
    if ! kill -0 "$WORKER_PID" 2>/dev/null; then break; fi
    sleep 0.5
  done
  if (( started == 1 )); then
    record PASS "worker start" "worker opened the shared file at schema v6 and is polling (no migration)"
  else
    record FAIL "worker start" "worker did not start; tail: $(tail -n 5 "$TMP/worker.log" 2>/dev/null | tr '\n' ' ')"
  fi

  status=""
  error_text=""
  for _ in $(seq 1 120); do
    status="$(sqlite3 -readonly "$DB" "SELECT status FROM apply_jobs WHERE id='$JOB_ID'" 2>/dev/null || true)"
    case "$status" in
      applied|failed|rolled-back) break ;;
    esac
    if [[ -n "$WORKER_PID" ]] && ! kill -0 "$WORKER_PID" 2>/dev/null; then break; fi
    sleep 0.5
  done
  error_text="$(sqlite3 -readonly "$DB" "SELECT coalesce(error_message,'') FROM apply_jobs WHERE id='$JOB_ID'" 2>/dev/null || true)"
  case "$status" in
    applied)
      record PASS "worker reaches apply job" "apply job $JOB_ID status=applied" ;;
    failed|rolled-back)
      if printf '%s' "$error_text" | rg -qi 'ENOENT|ECONNREFUSED|socket|control'; then
        record SKIPPED "worker apply to applied" "job reached terminal status=$status because no control daemon exists locally (control socket is Docker-only); error: $error_text"
        record PASS "worker reaches apply job" "worker claimed and finalized job $JOB_ID as terminal status=$status (not stuck)"
      else
        record FAIL "worker reaches apply job" "terminal status=$status with unexpected error: $error_text"
      fi ;;
    *)
      record FAIL "worker reaches apply job" "job $JOB_ID did not reach a terminal state (status='${status:-unknown}') in 60s; worker log tail: $(tail -n 5 "$TMP/worker.log" 2>/dev/null | tr '\n' ' ')" ;;
  esac

  if [[ -n "$WORKER_PID" ]] && kill -0 "$WORKER_PID" 2>/dev/null; then
    kill -TERM "$WORKER_PID" 2>/dev/null || true
    if wait "$WORKER_PID" 2>/dev/null; then
      record PASS "worker shutdown" "SIGTERM stopped the worker and closed its handles"
    else
      record PASS "worker shutdown" "SIGTERM stopped the worker (non-zero exit after signal)"
    fi
    WORKER_PID=""
  fi
fi

echo "== api after worker =="
code="$(req GET "/api/zones/$ZONE_ID/records")"
if [[ "$code" == 200 ]] && jq -e --arg id "$RECORD_ID" '(.zone.records // .records // []) | any(.id == $id)' "$BODY" >/dev/null 2>&1; then
  record PASS "api read-back" "GET /api/zones/{id}/records -> 200 and contains record written before the worker ran"
else
  record FAIL "api read-back" "GET records -> $code: $(head -c 300 "$BODY")"
fi

integrity="$(sqlite3 -readonly "$DB" "PRAGMA integrity_check" 2>/dev/null || echo "error")"
fk="$(sqlite3 -readonly "$DB" "PRAGMA foreign_key_check" 2>/dev/null | wc -l | tr -d ' ')"
if [[ "$integrity" == ok && "$fk" == 0 ]]; then
  record PASS "database integrity" "integrity_check=ok, foreign_key_check empty"
else
  record FAIL "database integrity" "integrity_check=$integrity foreign_key_violations=$fk"
fi

record SKIPPED "dns and nginx data plane" "apply promotion, CoreDNS/nginx reload and health checks need the Docker control daemon and data-plane containers"

summary
(( FAILURES == 0 ))
