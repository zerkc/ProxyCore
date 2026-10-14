#!/usr/bin/env bash
# Docker E2E: a NODE enrolls against a PRIMARY that serves a private-CA
# certificate, loading the primary CA from the enrollment form API, then keeps
# syncing with the stored CA; finally the PRIMARY is rebuilt at the same address
# (new CA) and the NODE re-enrolls with the new CA replacing the old one.
#
# Everything is isolated: compose projects pcca-primary / pcca-node, the docker
# network pcca-net and images pcca-e2e-*:local. All of it is removed on exit
# (set PCCA_KEEP=1 to keep it for inspection). Exits non-zero on any FAIL.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE_FILE="$ROOT/scripts/e2e-node-ca-trust.compose.yaml"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/pcca-e2e.XXXXXX")"
PRIMARY_PORT="${PCCA_PRIMARY_PORT:-39001}"
NODE_PORT="${PCCA_NODE_PORT:-39002}"
PRIMARY_ALIAS="primary.pcca.test"
NODE_ALIAS="node.pcca.test"
PRIMARY_URL="https://$PRIMARY_ALIAS:3443/"
PASSWORD="$(openssl rand -hex 18)"
KEY_PRIMARY="$(openssl rand -base64 32)"
KEY_NODE="$(openssl rand -base64 32)"
RESULTS=()
FAILURES=0
BODY="$TMP/body.json"

record() { RESULTS+=("$1|$2|$3"); printf '[%s] %s: %s\n' "$1" "$2" "$3"; [[ "$1" == FAIL ]] && FAILURES=$((FAILURES + 1)) || true; }
summary() {
  echo; echo "== node-ca-trust docker e2e summary =="
  local row; for row in "${RESULTS[@]}"; do IFS='|' read -r s n d <<<"$row"; printf '%-5s %s -- %s\n' "$s" "$n" "$d"; done
  if ((FAILURES > 0)); then echo "RESULT: FAIL ($FAILURES)"; else echo "RESULT: PASS"; fi
}
bail() { record FAIL "$1" "$2"; summary; exit 1; }

dc() { # which args...
  local which="$1"; shift
  local port alias key
  if [[ "$which" == primary ]]; then port=$PRIMARY_PORT alias=$PRIMARY_ALIAS key=$KEY_PRIMARY; else port=$NODE_PORT alias=$NODE_ALIAS key=$KEY_NODE; fi
  PCCA_WEB_PORT=$port PCCA_ALIAS=$alias PCCA_MASTER_KEY=$key docker compose -f "$COMPOSE_FILE" -p "pcca-$which" "$@"
}

cleanup() {
  if [[ "${PCCA_KEEP:-0}" == 1 ]]; then echo "PCCA_KEEP=1: leaving pcca-* projects, network and images in place"; rm -rf "$TMP"; return; fi
  dc primary down -v --remove-orphans >/dev/null 2>&1 || true
  dc node down -v --remove-orphans >/dev/null 2>&1 || true
  docker network rm pcca-net >/dev/null 2>&1 || true
  docker image rm pcca-e2e-api:local pcca-e2e-runtime:local >/dev/null 2>&1 || true
  rm -rf "$TMP"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

for cmd in docker curl jq openssl; do command -v "$cmd" >/dev/null || bail prerequisites "missing $cmd"; done

# req WHICH METHOD PATH [JSON] -> status code on stdout, body in $BODY.
req() {
  local which="$1" method="$2" path="$3" data="${4-}" port
  [[ "$which" == primary ]] && port=$PRIMARY_PORT || port=$NODE_PORT
  local args=(-sS -o "$BODY" -w '%{http_code}' -X "$method" -b "$TMP/$which.jar" -c "$TMP/$which.jar" "http://127.0.0.1:$port$path")
  [[ -n "$data" ]] && args+=(-H 'content-type: application/json' --data "$data")
  curl "${args[@]}" || echo 000
}
expect() { # name want got detail
  if [[ "$3" == "$2" ]]; then record PASS "$1" "$4 -> $3"; else bail "$1" "$4 -> $3 (want $2): $(head -c 300 "$BODY")"; fi
}
wait_health() {
  local which="$1" i
  for i in $(seq 1 90); do [[ "$(req "$which" GET /api/health)" == 200 ]] && return 0; sleep 1; done
  bail "health $which" "api did not become healthy"
}
install() { # which -> up, bootstrap owner, log in
  local which="$1"
  dc "$which" up -d >/dev/null 2>&1 || bail "up $which" "docker compose up failed"
  wait_health "$which"
  rm -f "$TMP/$which.jar"
  expect "bootstrap $which" 201 "$(req "$which" POST /api/auth/bootstrap "$(jq -nc --arg p "$PASSWORD" '{username:"owner",password:$p}')")" "POST /api/auth/bootstrap"
  expect "login $which" 200 "$(req "$which" POST /api/auth/login "$(jq -nc --arg p "$PASSWORD" '{username:"owner",password:$p}')")" "POST /api/auth/login"
}
configure_primary() { # hostname for the enrollment certificate, ingress, a zone and one apply
  local zone="$1"
  expect "primary hostnames" 200 "$(req primary PUT /api/settings/enrollment-hostnames "{\"hostnames\":[\"$PRIMARY_ALIAS\"]}")" "PUT /api/settings/enrollment-hostnames"
  expect "primary settings" 200 "$(req primary PUT /api/settings '{"ingress":{"ipv4":"192.0.2.10"},"defaultPool":{"id":"default","endpoints":[{"host":"192.0.2.53","port":53}]}}')" "PUT /api/settings"
  expect "primary zone $zone" 201 "$(req primary POST /api/zones "{\"name\":\"$zone\"}")" "POST /api/zones"
  expect "primary apply" 202 "$(req primary POST /api/apply)" "POST /api/apply"
}
# The enrollment listener only serves once a token has activated the primary,
# so wait for trust AFTER creating the token.
wait_trust() {
  local i status=""
  for i in $(seq 1 60); do
    req primary GET /api/topology/settings/enrollment-trust >/dev/null; status="$(jq -r '.status // empty' "$BODY")"
    [[ "$status" == ready ]] && break; sleep 1
  done
  [[ "$status" == ready ]] && record PASS "primary enrollment trust" "status ready" || bail "primary enrollment trust" "status=$status"
}
new_token() { req primary POST /api/enrollment/tokens '{}' >/dev/null; jq -r '.token // .Token // empty' "$BODY"; }
primary_ca() { req primary GET /api/topology/settings/enrollment-trust/ca >/dev/null; cp "$BODY" "$TMP/ca.pem"; req primary GET /api/topology/settings/enrollment-trust >/dev/null; jq -r '.caDerSha256' "$BODY"; }
draft() { # token [caPem] -> status
  local token="$1" ca="${2-}"
  if [[ -n "$ca" ]]; then req node POST /api/topology/enrollment/draft "$(jq -nc --arg t "$token" --arg u "$PRIMARY_URL" --arg c "$ca" '{token:$t,primaryURL:$u,caPem:$c}')"
  else req node POST /api/topology/enrollment/draft "$(jq -nc --arg t "$token" --arg u "$PRIMARY_URL" '{token:$t,primaryURL:$u}')"; fi
}
draft_retry() { # token ca -> status (waits for the primary to publish its first snapshot)
  local i code
  for i in $(seq 1 45); do code="$(draft "$1" "$2")"; [[ "$code" == 200 ]] && break; sleep 1; done
  echo "$code"
}
wait_node_synced() { # want zone name
  local zone="$1" i
  for i in $(seq 1 60); do
    req node GET /api/zones >/dev/null
    if jq -e --arg z "$zone" '(if type=="array" then . else (.zones // .items // []) end) | map(select(.name==$z)) | length > 0' "$BODY" >/dev/null 2>&1; then return 0; fi
    sleep 1
  done
  return 1
}

echo "== build =="
docker network inspect pcca-net >/dev/null 2>&1 || docker network create pcca-net >/dev/null
dc primary build >/dev/null 2>&1 && record PASS "build images" "pcca-e2e-api/runtime from the working tree" || bail "build images" "docker compose build failed"

echo "== A. node cannot trust a private-CA primary until the CA is loaded =="
install primary; install node
configure_primary "first.pcca.test"
TOKEN="$(new_token)"; [[ -n "$TOKEN" ]] || bail "token" "no enrollment token returned"
wait_trust
FP="$(primary_ca)"; [[ "$FP" =~ ^[0-9a-f]{64}$ ]] || bail "primary CA" "no fingerprint: $FP"
record PASS "primary CA" "downloaded, fingerprint $FP"

code="$(draft "$TOKEN")"
if [[ "$code" == 409 ]] && jq -e '.error | test("not trusted")' "$BODY" >/dev/null; then record PASS "no CA -> untrusted" "draft 409: $(jq -r .error "$BODY")"
else bail "no CA -> untrusted" "draft -> $code: $(head -c 300 "$BODY")"; fi
code="$(draft "$TOKEN" "not a certificate")"
[[ "$code" == 400 ]] && record PASS "invalid CA rejected" "draft 400: $(jq -r .error "$BODY")" || bail "invalid CA rejected" "draft -> $code: $(head -c 300 "$BODY")"
code="$(draft_retry "$TOKEN" "$(cat "$TMP/ca.pem")")"
expect "draft with CA" 200 "$code" "POST /api/topology/enrollment/draft caPem"
SHOWN="$(jq -r '.primaryCaFingerprint // empty' "$BODY")"; DRAFT_ID="$(jq -r '.draftId' "$BODY")"
[[ "$SHOWN" == "$FP" ]] && record PASS "fingerprint shown to the owner" "$SHOWN" || bail "fingerprint shown to the owner" "got '$SHOWN' want $FP"
expect "confirm" 200 "$(req node POST /api/topology/enrollment/confirm "{\"draftId\":\"$DRAFT_ID\"}")" "POST /api/topology/enrollment/confirm"
record PASS "node role" "$(jq -r '.role' "$BODY")"

echo "== B. continuous sync keeps verifying the primary with the stored CA =="
expect "primary second zone" 201 "$(req primary POST /api/zones '{"name":"second.pcca.test"}')" "POST /api/zones"
expect "primary re-apply" 202 "$(req primary POST /api/apply)" "POST /api/apply"
wait_node_synced "second.pcca.test" && record PASS "edit reaches node" "second.pcca.test replicated through the stored CA" || bail "edit reaches node" "second.pcca.test never reached the node"
req node GET /api/topology/sync >/dev/null
record PASS "node sync status" "$(jq -c '{bound,outdated,acknowledged,lastError}' "$BODY")"

echo "== C. primary rebuilt at the same address (new CA) -> re-enroll replaces the stored CA =="
OLD_CA="$(cat "$TMP/ca.pem")"
dc primary down -v >/dev/null 2>&1
install primary
configure_primary "rebuilt.pcca.test"
TOKEN2="$(new_token)"; wait_trust; FP2="$(primary_ca)"
[[ "$FP2" != "$FP" ]] && record PASS "new primary has a new CA" "$FP2" || bail "new primary has a new CA" "fingerprint unchanged"
# A NODE that holds a cluster key is bound to its primary and is never re-pointed
# silently; the supported path is Promote to PRIMARY, then Convert to NODE.
expect "node promote" 200 "$(req node POST /api/topology/promote '{"confirm":true}')" "POST /api/topology/promote"
expect "node convert to NODE" 200 "$(req node POST /api/topology/demote '{"confirm":true}')" "POST /api/topology/demote"
code="$(draft "$TOKEN2")"
if [[ "$code" == 409 ]] && jq -e '.error | test("not trusted")' "$BODY" >/dev/null; then record PASS "stored old CA does not trust the new primary" "draft 409"
else bail "stored old CA does not trust the new primary" "draft -> $code: $(head -c 300 "$BODY")"; fi
code="$(draft "$TOKEN2" "$OLD_CA")"
[[ "$code" == 409 ]] && record PASS "old CA loaded again is still refused" "draft 409" || bail "old CA loaded again is still refused" "draft -> $code: $(head -c 300 "$BODY")"
code="$(draft_retry "$TOKEN2" "$(cat "$TMP/ca.pem")")"
expect "draft with the new CA" 200 "$code" "POST /api/topology/enrollment/draft caPem(new)"
DRAFT2="$(jq -r '.draftId' "$BODY")"
code="$(req node POST /api/topology/enrollment/confirm "{\"draftId\":\"$DRAFT2\"}")"
expect "re-enroll confirm" 200 "$code" "POST /api/topology/enrollment/confirm"
expect "rebuilt primary edit" 201 "$(req primary POST /api/zones '{"name":"after-rebuild.pcca.test"}')" "POST /api/zones"
expect "rebuilt primary apply" 202 "$(req primary POST /api/apply)" "POST /api/apply"
wait_node_synced "after-rebuild.pcca.test" && record PASS "sync with the replaced CA" "after-rebuild.pcca.test replicated" || bail "sync with the replaced CA" "edit never reached the node"

echo "== log hygiene =="
if { dc primary logs --no-color 2>&1; dc node logs --no-color 2>&1; } | grep -E "pcenr1_|BEGIN (EC |RSA )?PRIVATE KEY|$PASSWORD" >/dev/null; then
  record FAIL "no secrets in logs" "a token, private key or password appears in container logs"
else record PASS "no secrets in logs" "no enrollment token, private key or password in either installation's logs"; fi

summary
((FAILURES == 0))
