#!/usr/bin/env bash
# Shared helpers for the Docker e2e scripts (sourced, not executed).
#
# The caller must set before sourcing: ROOT, COMPOSE_FILE, E2E_PREFIX (compose
# project / network / image prefix, e.g. "pcep"), E2E_TITLE, PRIMARY_PORT,
# NODE_PORT, PRIMARY_TLS_PORT, NODE_TLS_PORT, PRIMARY_ALIAS, NODE_ALIAS.
# Provides: record/summary/bail, dc, req/treq/expect/jqexpect, install, cleanup.
# Every request is logged to $TMP/hits.txt as "METHOD path" for the coverage check.

TMP="$(mktemp -d "${TMPDIR:-/tmp}/${E2E_PREFIX}-e2e.XXXXXX")"
PASSWORD="$(openssl rand -hex 18)"
KEY_PRIMARY="$(openssl rand -base64 32)"
KEY_NODE="$(openssl rand -base64 32)"
RESULTS=()
FAILURES=0
BODY="$TMP/body.json"
HITS="$TMP/hits.txt"
: >"$HITS"

record() { RESULTS+=("$1|$2|$3"); printf '[%s] %s: %s\n' "$1" "$2" "$3"; [[ "$1" == FAIL ]] && FAILURES=$((FAILURES + 1)) || true; }
summary() {
  echo; echo "== $E2E_TITLE summary =="
  local row s n d
  for row in "${RESULTS[@]}"; do IFS='|' read -r s n d <<<"$row"; printf '%-5s %s -- %s\n' "$s" "$n" "$d"; done
  if ((FAILURES > 0)); then echo "RESULT: FAIL ($FAILURES)"; else echo "RESULT: PASS (${#RESULTS[@]} checks)"; fi
}
bail() { record FAIL "$1" "$2"; summary; exit 1; }

dc() { # which args...
  local which="$1"; shift
  local port tls alias key
  if [[ "$which" == primary ]]; then port=$PRIMARY_PORT tls=$PRIMARY_TLS_PORT alias=$PRIMARY_ALIAS key=$KEY_PRIMARY
  else port=$NODE_PORT tls=$NODE_TLS_PORT alias=$NODE_ALIAS key=$KEY_NODE; fi
  PCEP_WEB_PORT=$port PCEP_TLS_PORT=$tls PCEP_ALIAS=$alias PCEP_MASTER_KEY=$key PCEP_WORKER_PREP="${E2E_WORKER_PREP:-true}" \
    docker compose -f "$COMPOSE_FILE" -p "$E2E_PREFIX-$which" "$@"
}

cleanup() {
  if [[ "${E2E_KEEP:-0}" == 1 ]]; then echo "E2E_KEEP=1: leaving $E2E_PREFIX-* projects, network and images in place"; return; fi
  dc primary down -v --remove-orphans >/dev/null 2>&1 || true
  dc node down -v --remove-orphans >/dev/null 2>&1 || true
  docker network rm "$E2E_PREFIX-net" >/dev/null 2>&1 || true
  docker image rm "$E2E_PREFIX-e2e-api:local" "$E2E_PREFIX-e2e-runtime:local" >/dev/null 2>&1 || true
  if [[ -n "${E2E_HITS_OUT:-}" ]]; then cp "$HITS" "$E2E_HITS_OUT" 2>/dev/null || true; fi
  rm -rf "$TMP"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

for cmd in docker curl jq openssl; do command -v "$cmd" >/dev/null || bail prerequisites "missing $cmd"; done

# req WHICH METHOD PATH [JSON] -> HTTP status on stdout, body in $BODY.
req() {
  local which="$1" method="$2" path="$3" data="${4-}" port
  [[ "$which" == primary ]] && port=$PRIMARY_PORT || port=$NODE_PORT
  echo "$method ${path%%\?*}" >>"$HITS"
  local args=(-sS -o "$BODY" -w '%{http_code}' -X "$method" -b "$TMP/$which.jar" -c "$TMP/$which.jar" "http://127.0.0.1:$port$path")
  [[ -n "$data" ]] && args+=(-H 'content-type: application/json' --data "$data")
  curl "${args[@]}" || echo 000
}
# rawreq WHICH METHOD PATH [CURL_ARGS...] -> status; no cookie jar, no body.
rawreq() {
  local which="$1" method="$2" path="$3"; shift 3
  local port; [[ "$which" == primary ]] && port=$PRIMARY_PORT || port=$NODE_PORT
  echo "$method ${path%%\?*}" >>"$HITS"
  curl -sS -o "$BODY" -w '%{http_code}' -X "$method" "$@" "http://127.0.0.1:$port$path" || echo 000
}
# treq WHICH METHOD PATH [JSON] -> status; enrollment TLS listener (:3443), TLS verification off
# (the e2e checks the endpoint contract; CA pinning and CA replacement are asserted in e2e-docker-endpoints.sh).
# The alias is resolved to 127.0.0.1 so SNI and Host match the enrollment certificate.
treq() {
  local which="$1" method="$2" path="$3" data="${4-}" port
  [[ "$which" == primary ]] && port=$PRIMARY_TLS_PORT || port=$NODE_TLS_PORT
  echo "$method ${path%%\?*}" >>"$HITS"
  local alias; [[ "$which" == primary ]] && alias=$PRIMARY_ALIAS || alias=$NODE_ALIAS
  local args=(-sSk -o "$BODY" -w '%{http_code}' -X "$method" --resolve "$alias:$port:127.0.0.1" "https://$alias:$port$path")
  [[ -n "$data" ]] && args+=(-H 'content-type: application/json' --data "$data")
  curl "${args[@]}" || echo 000
}
expect() { # name want got detail
  if [[ "$3" == "$2" ]]; then record PASS "$1" "$4 -> $3"; else bail "$1" "$4 -> $3 (want $2): $(head -c 400 "$BODY")"; fi
}
# jqexpect NAME FILTER [FILE]: the jq filter must be true over the last body.
jqexpect() {
  local file="${3:-$BODY}"
  if jq -e "$2" "$file" >/dev/null 2>&1; then record PASS "$1" "$2"; else bail "$1" "jq '$2' false over: $(head -c 400 "$file")"; fi
}
expect_any() { # name "want1 want2" got detail
  local w; for w in $2; do [[ "$3" == "$w" ]] && { record PASS "$1" "$4 -> $3"; return 0; }; done
  bail "$1" "$4 -> $3 (want one of: $2): $(head -c 400 "$BODY")"
}
wait_health() {
  local which="$1" i
  for i in $(seq 1 90); do [[ "$(req "$which" GET /api/health)" == 200 ]] && return 0; sleep 1; done
  bail "health $which" "api did not become healthy"
}
install() { # which -> up (no bootstrap)
  local which="$1"
  dc "$which" up -d >/dev/null 2>&1 || bail "up $which" "docker compose up failed"
  wait_health "$which"
  rm -f "$TMP/$which.jar"
}
login_body() { jq -nc --arg p "${2:-$PASSWORD}" --arg u "${1:-owner}" '{username:$u,password:$p}'; }
bootstrap_login() { # which
  local which="$1"
  expect "bootstrap $which" 201 "$(req "$which" POST /api/auth/bootstrap "$(login_body)")" "POST /api/auth/bootstrap"
  expect "login $which" 200 "$(req "$which" POST /api/auth/login "$(login_body)")" "POST /api/auth/login"
}
build_images() {
  docker network inspect "$E2E_PREFIX-net" >/dev/null 2>&1 || docker network create "$E2E_PREFIX-net" >/dev/null
  dc primary build >/dev/null 2>&1 && record PASS "build images" "$E2E_PREFIX-e2e-api/runtime from the working tree" || bail "build images" "docker compose build failed"
}
