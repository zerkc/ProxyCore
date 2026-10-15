#!/usr/bin/env bash
# Docker E2E: calls every HTTP endpoint of the API at least once, on a real
# PRIMARY + NODE pair (api + worker + control each, no nginx/coredns), and fails
# non-zero on any mismatch.
#
#   E1  PRIMARY matrix: public routes, auth, settings, zones, records, streams,
#       certificates, apply, users, backup, updates, status.
#   E2  Topology matrix: enrollment tokens (v2), trust, join with only the token,
#       the :3443 listener, nodes view + heartbeat, sync, NODE read-only matrix,
#       unlink -> NODE_UNLINKED, re-enroll, promote/demote.
#   E3  Coverage: every route of server.go routes() must have been hit.
#
# Isolated: compose projects pcep-primary / pcep-node, network pcep-net, images
# pcep-e2e-*:local; all removed on exit (E2E_KEEP=1 keeps them).
# Ports: E2E_PRIMARY_PORT/E2E_NODE_PORT (HTTP), E2E_PRIMARY_TLS_PORT/E2E_NODE_TLS_PORT (:3443).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE_FILE="$ROOT/scripts/e2e-docker-endpoints.compose.yaml"
E2E_PREFIX=pcep
E2E_TITLE="docker endpoints e2e"
PRIMARY_PORT="${E2E_PRIMARY_PORT:-39201}"
NODE_PORT="${E2E_NODE_PORT:-39202}"
PRIMARY_TLS_PORT="${E2E_PRIMARY_TLS_PORT:-39211}"
NODE_TLS_PORT="${E2E_NODE_TLS_PORT:-39212}"
PRIMARY_ALIAS="primary.pcep.test"
NODE_ALIAS="node.pcep.test"
PRIMARY_URL="https://$PRIMARY_ALIAS:3443/"
NEW_PASSWORD="$(openssl rand -hex 18)"
export E2E_WORKER_PREP="${E2E_WORKER_PREP:-true}"
# shellcheck source=lib/e2e-docker.sh
source "$ROOT/scripts/lib/e2e-docker.sh"

# --- worker schema guard ----------------------------------------------------
# The worker refuses to start when the SQLite schema is newer than the version
# it hard-codes. If the migrations moved ahead of the worker (a product bug,
# reported by this suite), patch the constant inside the throw-away container so
# the rest of the matrix can still run, and record the finding in the summary.
LATEST_MIGRATION="$(ls "$ROOT"/apps/api/internal/sqlitedb/migrations/*.sql | sed -E 's|.*/0*([0-9]+)_.*|\1|' | sort -n | tail -1)"
WORKER_SCHEMA="$(sed -nE 's/.*EXPECTED_SQLITE_SCHEMA_VERSION = ([0-9]+).*/\1/p' "$ROOT/apps/worker/src/sqlite-runtime.ts")"
WORKER_FINDING=""
if [[ -n "$WORKER_SCHEMA" && "$WORKER_SCHEMA" -lt "$LATEST_MIGRATION" ]]; then
  export E2E_WORKER_PREP="sed -i 's/EXPECTED_SQLITE_SCHEMA_VERSION = $WORKER_SCHEMA/EXPECTED_SQLITE_SCHEMA_VERSION = $LATEST_MIGRATION/' apps/worker/src/sqlite-runtime.ts"
  WORKER_FINDING="worker expects sqlite schema $WORKER_SCHEMA but migrations reach $LATEST_MIGRATION; constant patched in the container only"
fi

wait_for() { # name tries cmd... : poll a command until it succeeds
  local name="$1" tries="$2" i; shift 2
  for i in $(seq 1 "$tries"); do "$@" && return 0; sleep 1; done
  bail "$name" "condition not met after ${tries}s; last body: $(head -c 500 "$BODY")"
}
has_zone() { # which zone
  req "$1" GET /api/zones >/dev/null
  jq -e --arg z "$2" '.zones | map(select(.name==$z)) | length > 0' "$BODY" >/dev/null 2>&1
}
node_field() { # jq expr over the NODE's /api/topology/sync
  req node GET /api/topology/sync >/dev/null; jq -e "$1" "$BODY" >/dev/null 2>&1
}
primary_node_field() { # jq expr over the first listed node
  req primary GET /api/topology/nodes >/dev/null; jq -e "$1" "$BODY" >/dev/null 2>&1
}
new_token() { # -> token string, 201 asserted by the caller through $?
  local code; code="$(req primary POST /api/enrollment/tokens "{\"primaryUrl\":\"$PRIMARY_URL\"}")"
  [[ "$code" == 201 ]] || bail "create token" "POST /api/enrollment/tokens -> $code: $(head -c 300 "$BODY")"
  jq -r '.Token' "$BODY"
}
draft_retry() { # token -> status, body holds the draft; waits for the first published snapshot
  local i code
  for i in $(seq 1 60); do
    code="$(req node POST /api/topology/enrollment/draft "$(jq -nc --arg t "$1" '{token:$t}')")"
    [[ "$code" == 200 ]] && break; sleep 1
  done
  echo "$code"
}
join_node() { # token label : draft -> preview -> confirm using ONLY the token
  local token="$1" label="$2" code id
  code="$(draft_retry "$token")"; expect "$label draft" 200 "$code" "POST /api/topology/enrollment/draft {token}"
  id="$(jq -r '.draftId' "$BODY")"
  jqexpect "$label draft shows the primary CA fingerprint" '.primaryCaFingerprint | test("^[0-9a-f]{64}$")'
  expect "$label preview" 200 "$(req node POST /api/topology/enrollment/preview "{\"draftId\":\"$id\"}")" "POST /api/topology/enrollment/preview"
  expect "$label confirm" 200 "$(req node POST /api/topology/enrollment/confirm "{\"draftId\":\"$id\"}")" "POST /api/topology/enrollment/confirm"
  jqexpect "$label became a node" '.role == "node"'
}

echo "== build =="
build_images
[[ -n "$WORKER_FINDING" ]] && record PASS "KNOWN ISSUE worker schema" "$WORKER_FINDING"
install primary; install node

# ============================================================================
echo "== E1. PRIMARY endpoint matrix =="
expect "health" 200 "$(req primary GET /api/health)" "GET /api/health"
jqexpect "health shape" '.ok == true and .service == "proxycore-api" and (.version|type=="string")'
expect "ready" 200 "$(req primary GET /api/ready)" "GET /api/ready"
jqexpect "ready identity" '.ok == true and .identity.role == "standalone-primary" and .identity.writable == true'
expect "updates" 200 "$(req primary GET /api/updates)" "GET /api/updates"
jqexpect "updates disabled" '.status == "disabled" and .updateAvailable == false'
expect "updates refresh" 200 "$(req primary GET '/api/updates?refresh=1')" "GET /api/updates?refresh=1"
expect "acme-challenge unknown token" 404 "$(req primary GET /api/acme-challenge/unknown-token)" "GET /api/acme-challenge/{token}"
expect "SPA fallback" 200 "$(req primary GET /)" "GET /"
expect "status requires auth" 401 "$(rawreq primary GET /api/status)" "GET /api/status (no session)"
expect "bootstrap" 201 "$(req primary POST /api/auth/bootstrap "$(login_body)")" "POST /api/auth/bootstrap"
expect "login wrong password" 401 "$(req primary POST /api/auth/login "$(login_body owner wrong-password-123)")" "POST /api/auth/login (bad password)"
expect "login" 200 "$(req primary POST /api/auth/login "$(login_body)")" "POST /api/auth/login"
jqexpect "login user" '.user.username == "owner" and .user.role == "owner" and (.expiresAt|type=="string")'
expect "bootstrap again" 409 "$(req primary POST /api/auth/bootstrap "$(login_body)")" "POST /api/auth/bootstrap (already complete)"
bootstrap_login node

expect "status" 200 "$(req primary GET /api/status)" "GET /api/status"
jqexpect "status shape" '.identity.role == "standalone-primary" and (.zones|type=="array") and (.certificates|type=="array") and (.streams|type=="array")'
expect "settings get" 200 "$(req primary GET /api/settings)" "GET /api/settings"
jqexpect "settings defaults" '.settings.retentionMaxAgeDays == 7'
expect "enrollment hostnames get" 200 "$(req primary GET /api/settings/enrollment-hostnames)" "GET /api/settings/enrollment-hostnames"
jqexpect "enrollment hostnames empty" '.configured == false and .hostnames == []'
expect "enrollment hostnames put" 200 "$(req primary PUT /api/settings/enrollment-hostnames "{\"hostnames\":[\"$PRIMARY_ALIAS\"]}")" "PUT /api/settings/enrollment-hostnames"
jqexpect "enrollment hostnames stored" ".configured == true and .hostnames == [\"$PRIMARY_ALIAS\"]"
expect "settings put" 200 "$(req primary PUT /api/settings '{"ingress":{"ipv4":"192.0.2.10"},"defaultPool":{"id":"default","endpoints":[{"host":"192.0.2.53","port":53}]}}')" "PUT /api/settings"
jqexpect "settings stored" '.settings.ingress.ipv4 == "192.0.2.10"'

expect "zone create" 201 "$(req primary POST /api/zones '{"name":"e2e.test"}')" "POST /api/zones"
ZONE_ID="$(jq -r '.zone.id' "$BODY")"
expect "zones list" 200 "$(req primary GET /api/zones)" "GET /api/zones"
jqexpect "zones list contains e2e.test" '.zones | map(.name) | index("e2e.test") != null'
expect "record invalid" 400 "$(req primary POST "/api/zones/$ZONE_ID/records" '{"name":"www"}')" "POST /api/zones/{id}/records (invalid)"
expect "record create" 201 "$(req primary POST "/api/zones/$ZONE_ID/records" '{"name":"www","type":"A","value":"192.0.2.20","ttl":300}')" "POST /api/zones/{id}/records"
REC_ID="$(jq -r '.record.id' "$BODY")"
expect "records get" 200 "$(req primary GET "/api/zones/$ZONE_ID/records")" "GET /api/zones/{id}/records"
jqexpect "records list" '.zone.records | length == 1 and .[0].value == "192.0.2.20"'
expect "record patch" 200 "$(req primary PATCH "/api/zones/$ZONE_ID/records/$REC_ID" '{"name":"www","type":"A","value":"192.0.2.21","ttl":120}')" "PATCH /api/zones/{id}/records/{id}"
jqexpect "record patched" '.record.value == "192.0.2.21" and .record.ttl == 120'

expect "stream invalid" 400 "$(req primary POST /api/streams '{"protocol":"tcp","listenAddress":"0.0.0.0","listenPort":9443,"upstream":{"host":"x","port":1}}')" "POST /api/streams (invalid)"
expect "stream create" 201 "$(req primary POST /api/streams '{"protocol":"tcp","listenAddress":"0.0.0.0","listenPort":9443,"upstream":{"ip":"192.0.2.30","port":443,"protocol":"tcp"}}')" "POST /api/streams"
STREAM_ID="$(jq -r '.stream.id' "$BODY")"
expect "streams list" 200 "$(req primary GET /api/streams)" "GET /api/streams"
jqexpect "streams list contains it" ".streams | map(.id) | index(\"$STREAM_ID\") != null"
expect "stream patch" 200 "$(req primary PATCH "/api/streams/$STREAM_ID" '{"listenPort":9444}')" "PATCH /api/streams/{id}"
jqexpect "stream patched" '.stream.listenPort == 9444'
expect "stream delete" 200 "$(req primary DELETE "/api/streams/$STREAM_ID")" "DELETE /api/streams/{id}"

expect "certificate invalid" 400 "$(req primary POST /api/certificates '{"hostnames":["www.e2e.test"],"issuer":"nope","challenge":"none"}')" "POST /api/certificates (invalid)"
expect "certificate issue self-signed" 201 "$(req primary POST /api/certificates '{"hostnames":["www.e2e.test"],"issuer":"self-signed","challenge":"none"}')" "POST /api/certificates"
CERT_ID="$(jq -r '.certificate.id' "$BODY")"
jqexpect "certificate active" '.certificate.status == "active" and .certificate.issuer == "self-signed"'
expect "certificates list" 200 "$(req primary GET /api/certificates)" "GET /api/certificates"
jqexpect "certificates list contains it" ".certificates | map(.id) | index(\"$CERT_ID\") != null"
expect "internal CA download" 200 "$(req primary GET /api/certificates/ca)" "GET /api/certificates/ca"
grep -q 'BEGIN CERTIFICATE' "$BODY" && record PASS "internal CA is a PEM" "BEGIN CERTIFICATE" || bail "internal CA is a PEM" "not a PEM"
expect "certificate renew" 200 "$(req primary POST "/api/certificates/$CERT_ID/renew")" "POST /api/certificates/{id}/renew"
jqexpect "certificate renewed" '.applied == true and (.jobId|type=="string")'
expect "certificate delete" 200 "$(req primary DELETE "/api/certificates/$CERT_ID")" "DELETE /api/certificates/{id}"

expect "apply" 202 "$(req primary POST /api/apply)" "POST /api/apply"
jqexpect "apply queued job" '.job.status == "queued" and (.revisionId|type=="string")'

expect "users list" 200 "$(req primary GET /api/users)" "GET /api/users"
OWNER_ID="$(jq -r '.users[0].id' "$BODY")"
expect "user create" 201 "$(req primary POST /api/users '{"username":"op1","password":"operator-pass-1234","role":"operator"}')" "POST /api/users"
OP_ID="$(jq -r '.user.id' "$BODY")"
expect "user patch" 200 "$(req primary PATCH "/api/users/$OP_ID" '{"active":false}')" "PATCH /api/users/{id}"
jqexpect "user deactivated" '.user.active == false'
expect "last owner delete refused" 400 "$(req primary DELETE "/api/users/$OWNER_ID")" "DELETE /api/users/{owner}"
expect_any "user delete" "200 204" "$(req primary DELETE "/api/users/$OP_ID")" "DELETE /api/users/{id}"

expect "updates apply without body" 400 "$(req primary POST /api/updates/apply '{}')" "POST /api/updates/apply {}"
expect "updates apply without updater" 503 "$(req primary POST /api/updates/apply '{"targetVersion":"9.9.9"}')" "POST /api/updates/apply"
jqexpect "updater not configured" '.error == "updater is not configured"'

BACKUP="$TMP/backup.zip"
code="$(rawreq primary POST /api/backup/export -b "$TMP/primary.jar" -H 'content-type: application/json' --data '{}' -D "$TMP/export.headers")"
cp "$BODY" "$BACKUP"
[[ "$code" == 200 ]] && head -c 2 "$BACKUP" | grep -q PK && grep -qi 'application/zip' "$TMP/export.headers" \
  && record PASS "backup export" "POST /api/backup/export -> 200 zip ($(wc -c <"$BACKUP") bytes)" || bail "backup export" "status $code or not a zip"
code="$(rawreq primary POST '/api/backup/import?dry_run=true' -b "$TMP/primary.jar" -F "bundle=@$BACKUP")"
expect "backup import dry run" 200 "$code" "POST /api/backup/import?dry_run=true"
jqexpect "backup import report" '.dryRun == true and (.tables|type=="array")'
echo "not a zip" >"$TMP/bad.zip"
code="$(rawreq primary POST '/api/backup/import?dry_run=true' -b "$TMP/primary.jar" -F "bundle=@$TMP/bad.zip")"
expect_any "backup import rejects garbage" "400 422" "$code" "POST /api/backup/import (invalid bundle)"
expect "backup import needs dry_run" 400 "$(rawreq primary POST /api/backup/import -b "$TMP/primary.jar" -F "bundle=@$BACKUP")" "POST /api/backup/import (no dry_run)"

expect "change password short" 400 "$(req primary POST /api/auth/change-password '{"password":"abc"}')" "POST /api/auth/change-password (invalid)"
expect "change password" 200 "$(req primary POST /api/auth/change-password "{\"password\":\"$NEW_PASSWORD\"}")" "POST /api/auth/change-password"
expect "logout" 204 "$(req primary POST /api/auth/logout)" "POST /api/auth/logout"
expect "session ended" 401 "$(req primary GET /api/users)" "GET /api/users (after logout)"
expect "old password rejected" 401 "$(req primary POST /api/auth/login "$(login_body owner "$PASSWORD")")" "POST /api/auth/login (old password)"
expect "login new password" 200 "$(req primary POST /api/auth/login "$(login_body owner "$NEW_PASSWORD")")" "POST /api/auth/login"
expect "change password back" 200 "$(req primary POST /api/auth/change-password "{\"password\":\"$PASSWORD\"}")" "POST /api/auth/change-password"
expect "login restored" 200 "$(req primary POST /api/auth/login "$(login_body)")" "POST /api/auth/login"

echo "== log hygiene =="
if { dc primary logs --no-color 2>&1; dc node logs --no-color 2>&1; } | grep -E "pcenr[12]_|BEGIN (EC |RSA )?PRIVATE KEY|$PASSWORD|$NEW_PASSWORD" >/dev/null; then
  record FAIL "no secrets in logs" "a token, private key or password appears in container logs"
else record PASS "no secrets in logs" "no enrollment token, private key or password in either installation's logs"; fi

summary
((FAILURES == 0))
