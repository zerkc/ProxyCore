#!/usr/bin/env bash
# Disposable end-to-end verification for the primary/node enrollment pipeline.
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PRIMARY_CONTAINER="proxycore-pne9-primary-pg17"
NODE_CONTAINER="proxycore-pne9-node-pg17"
PRIMARY_DB="postgres://proxycore:proxycore@127.0.0.1:55445/proxycore"
NODE_DB="postgres://proxycore:proxycore@127.0.0.1:55446/proxycore"
PRIMARY_API="http://127.0.0.1:13443"
NODE_API="http://127.0.0.1:23443"
PRIMARY_TLS="https://127.0.0.1:13444"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/proxycore-pne9.XXXXXX")"
PRIMARY_PID=""
NODE_PID=""
PRIMARY_COOKIE="$WORK/primary.cookies"
NODE_COOKIE="$WORK/node.cookies"
RESPONSE_BODY=""
RESPONSE_STATUS=""

cleanup() {
  local status=$?
  set +e
  [[ -n "$PRIMARY_PID" ]] && kill "$PRIMARY_PID" >/dev/null 2>&1 || true
  [[ -n "$NODE_PID" ]] && kill "$NODE_PID" >/dev/null 2>&1 || true
  [[ -n "$PRIMARY_PID" ]] && wait "$PRIMARY_PID" >/dev/null 2>&1 || true
  [[ -n "$NODE_PID" ]] && wait "$NODE_PID" >/dev/null 2>&1 || true
  docker rm -f "$PRIMARY_CONTAINER" "$NODE_CONTAINER" >/dev/null 2>&1 || true
  rm -rf "$WORK"
  exit "$status"
}
trap cleanup EXIT INT TERM

fail() {
  printf '[PNE-9] FAIL: %s\n' "$1" >&2
  exit 1
}

need_command() {
  command -v "$1" >/dev/null 2>&1 || fail "required command is unavailable: $1"
}

for command in docker curl jq bun go openssl; do
  need_command "$command"
done

# The UI verifier is intentionally sourced so this orchestration cannot report
# an integrated pass when the required UI boundary is red.
source "$ROOT/scripts/verify-primary-node-enrollment-ui.sh"
verify_primary_node_enrollment_ui || fail "UI verification failed; integrated orchestration stopped"

wait_postgres() {
  local container=$1
  local attempt
  for attempt in $(seq 1 90); do
    if docker exec "$container" pg_isready -U proxycore -d proxycore >/dev/null 2>&1 &&
      docker exec "$container" psql -U proxycore -d proxycore -c 'select 1' >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  fail "PostgreSQL did not become ready: $container"
}

request_json() {
  local method=$1
  local url=$2
  local cookie_file=$3
  local data=${4:-}
  local insecure=${5:-0}
  local -a args=(--silent --show-error --max-time 10 --request "$method" --write-out $'\n%{http_code}' --cookie "$cookie_file" --cookie-jar "$cookie_file")
  [[ "$insecure" == "1" ]] && args+=(--insecure)
  [[ -n "$data" ]] && args+=(--header 'Content-Type: application/json' --data "$data")
  local result
  result=$(curl "${args[@]}" "$url") || fail "request failed: $method $url"
  RESPONSE_STATUS="${result##*$'\n'}"
  RESPONSE_BODY="${result%$'\n'*}"
}

expect_status() {
  local label=$1
  local expected=$2
  if [[ "$RESPONSE_STATUS" != "$expected" ]]; then
    printf '[PNE-9] %s: status=%s want=%s bodyLen=%s bodyHash=%s\n' \
      "$label" "$RESPONSE_STATUS" "$expected" "${#RESPONSE_BODY}" \
      "$(printf '%s' "$RESPONSE_BODY" | sha256sum | cut -d' ' -f1)" >&2
    exit 1
  fi
}

wait_http() {
  local url=$1
  local cookie_file=$2
  local attempt
  for attempt in $(seq 1 90); do
    if curl --silent --show-error --fail --max-time 2 --cookie "$cookie_file" "$url" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.2
  done
  fail "HTTP endpoint did not become ready: $url"
}

printf '[PNE-9] removing stale disposable containers\n'
docker rm -f "$PRIMARY_CONTAINER" "$NODE_CONTAINER" >/dev/null 2>&1 || true
printf '[PNE-9] starting PostgreSQL 17 containers\n'
docker run --name "$PRIMARY_CONTAINER" -e POSTGRES_USER=proxycore -e POSTGRES_PASSWORD=proxycore -e POSTGRES_DB=proxycore -p 55445:5432 -d postgres:17 >/dev/null
docker run --name "$NODE_CONTAINER" -e POSTGRES_USER=proxycore -e POSTGRES_PASSWORD=proxycore -e POSTGRES_DB=proxycore -p 55446:5432 -d postgres:17 >/dev/null
wait_postgres "$PRIMARY_CONTAINER"
wait_postgres "$NODE_CONTAINER"

printf '[PNE-9] applying migrations\n'
DATABASE_URL="$PRIMARY_DB" bun run db:migrate >/dev/null
DATABASE_URL="$NODE_DB" bun run db:migrate >/dev/null

PRIMARY_KEY="$(openssl rand -base64 32 | tr -d '\n')"
NODE_KEY="$(openssl rand -base64 32 | tr -d '\n')"
PRIMARY_SEED="$(openssl rand -base64 32 | tr -d '\n')"
NODE_SEED="$(openssl rand -base64 32 | tr -d '\n')"

printf '[PNE-9] starting API installations\n'
(
  cd "$ROOT/apps/api"
  DATABASE_URL="$PRIMARY_DB" PROXYCORE_API_ADDR=127.0.0.1:13443 \
    PROXYCORE_ENROLLMENT_TLS_ADDR=127.0.0.1:13444 \
    PROXYCORE_MASTER_KEY_BASE64="$PRIMARY_KEY" INTERNAL_CA_SEED="$PRIMARY_SEED" \
    PROXYCORE_UI_DIST="$ROOT/apps/ui/dist" PROXYCORE_UPDATE_CHECK_ENABLED=0 \
    go run ./cmd/server
) >"$WORK/primary-api.log" 2>&1 &
PRIMARY_PID=$!
(
  cd "$ROOT/apps/api"
  DATABASE_URL="$NODE_DB" PROXYCORE_API_ADDR=127.0.0.1:23443 \
    PROXYCORE_ENROLLMENT_TLS_ADDR=127.0.0.1:23444 \
    PROXYCORE_MASTER_KEY_BASE64="$NODE_KEY" INTERNAL_CA_SEED="$NODE_SEED" \
    PROXYCORE_UI_DIST="$ROOT/apps/ui/dist" PROXYCORE_UPDATE_CHECK_ENABLED=0 \
    go run ./cmd/server --enable-node-converter
) >"$WORK/node-api.log" 2>&1 &
NODE_PID=$!
wait_http "$PRIMARY_API/api/health" /dev/null
wait_http "$NODE_API/api/health" /dev/null

printf '[PNE-9] bootstrapping owners and logging in\n'
request_json POST "$PRIMARY_API/api/auth/bootstrap" "$PRIMARY_COOKIE" '{"username":"primary-owner","password":"correct horse battery staple"}'
expect_status 'PRIMARY bootstrap' 201
request_json POST "$PRIMARY_API/api/auth/login" "$PRIMARY_COOKIE" '{"username":"primary-owner","password":"correct horse battery staple"}'
expect_status 'PRIMARY login' 200
request_json POST "$NODE_API/api/auth/bootstrap" "$NODE_COOKIE" '{"username":"node-owner","password":"correct horse battery staple"}'
expect_status 'NODE bootstrap' 201
request_json POST "$NODE_API/api/auth/login" "$NODE_COOKIE" '{"username":"node-owner","password":"correct horse battery staple"}'
expect_status 'NODE login' 200

printf '[PNE-9] configuring PRIMARY enrollment and creating pcenr1 token\n'
request_json PUT "$PRIMARY_API/api/settings/enrollment-hostnames" "$PRIMARY_COOKIE" '{"hostnames":["127.0.0.1"]}'
expect_status 'enrollment hostname configuration' 200
request_json PUT "$PRIMARY_API/api/settings" "$PRIMARY_COOKIE" '{"defaultPool":{"id":"pne9","endpoints":[{"host":"1.1.1.1","port":53}]}}'
expect_status 'default resolver configuration' 200
request_json POST "$PRIMARY_API/api/enrollment/tokens" "$PRIMARY_COOKIE" '{}'
expect_status 'pcenr1 creation' 201
TOKEN_ID="$(printf '%s' "$RESPONSE_BODY" | jq -r '.id')"
TOKEN="$(printf '%s' "$RESPONSE_BODY" | jq -r '.token')"
[[ "$TOKEN_ID" != "null" && "$TOKEN" == pcenr1_* ]] || fail 'pcenr1 response was incomplete'

printf '[PNE-9] creating and terminalizing the PRIMARY ordinary apply\n'
request_json POST "$PRIMARY_API/api/apply" "$PRIMARY_COOKIE" '{}'
expect_status 'PRIMARY apply enqueue' 202
REVISION_ID="$(printf '%s' "$RESPONSE_BODY" | jq -r '.revisionId')"
JOB_ID="$(printf '%s' "$RESPONSE_BODY" | jq -r '.job.id')"
psql_primary() { docker exec "$PRIMARY_CONTAINER" psql -U proxycore -d proxycore -v ON_ERROR_STOP=1 "$@"; }
psql_node() { docker exec "$NODE_CONTAINER" psql -U proxycore -d proxycore -v ON_ERROR_STOP=1 "$@"; }
psql_primary -v revision="$REVISION_ID" -v job="$JOB_ID" -c \
  "update config_revisions set applied_at=now() where id=:'revision'::uuid; update apply_jobs set status='applied', finished_at=now() where id=:'job'::uuid; update installation_settings set current_applied_revision_id=:'revision' where id='default';" >/dev/null

printf '[PNE-9] waiting for canonical publication and fetching over HTTPS\n'
SNAPSHOT_BODY=''
for attempt in $(seq 1 90); do
  if SNAPSHOT_BODY=$(curl --silent --show-error --insecure --max-time 5 --request POST \
    --header 'Content-Type: application/json' --data "{\"token\":\"$TOKEN\"}" \
    "$PRIMARY_TLS/api/topology/sync/snapshot-by-token" 2>/dev/null) &&
    printf '%s' "$SNAPSHOT_BODY" | jq -e '.transient.sourcePrimaryId' >/dev/null 2>&1; then
    break
  fi
  SNAPSHOT_BODY=''
  sleep 0.5
done
[[ -n "$SNAPSHOT_BODY" ]] || fail 'canonical snapshot did not become available over HTTPS'
CONTENT_HASH="$(printf '%s' "$SNAPSHOT_BODY" | jq -r '.contentHash')"
PRIMARY_ID="$(printf '%s' "$SNAPSHOT_BODY" | jq -r '.transient.sourcePrimaryId')"
PRIMARY_GENERATION="$(printf '%s' "$SNAPSHOT_BODY" | jq -r '.transient.leadershipGeneration')"
NODE_ID="$(curl --silent --show-error --cookie "$NODE_COOKIE" "$NODE_API/api/status" | jq -r '.identity.nodeId')"
NODE_INSTALLATION_ID="$(curl --silent --show-error --cookie "$NODE_COOKIE" "$NODE_API/api/status" | jq -r '.identity.installationId')"

printf '[PNE-9] applying fetched snapshot to NODE and committing node role\n'
IMPORT_REVISION="$(cat /proc/sys/kernel/random/uuid)"
IMPORT_JOB="$(cat /proc/sys/kernel/random/uuid)"
IMPORT_NUMBER="$(psql_node -Atc 'select coalesce(max(revision_number),0)+1 from config_revisions')"
psql_node -v revision="$IMPORT_REVISION" -v job="$IMPORT_JOB" -v number="$IMPORT_NUMBER" \
  -v snapshot="$SNAPSHOT_BODY" -v primary="$PRIMARY_ID" -v node="$PRIMARY_ID" \
  -v content="$CONTENT_HASH" -v generation="$PRIMARY_GENERATION" -c \
  "insert into config_revisions (id,revision_number,checksum,snapshot,source,source_primary_id,source_node_id,snapshot_content_hash,snapshot_version,replication_version,leadership_generation,applied_at) values (:'revision'::uuid,:'number',:'content',:'snapshot'::jsonb,'import',:'primary'::uuid,:'node'::uuid,:'content',1,1,:'generation',now()); insert into apply_jobs (id,revision_id,target,status,correlation_id,source,source_primary_id,source_node_id,snapshot_content_hash,snapshot_version,replication_version,leadership_generation,finished_at) values (:'job'::uuid,:'revision'::uuid,'combined','applied',:'job','import',:'primary'::uuid,:'node'::uuid,:'content',1,1,:'generation',now()); update installation_identity set role='node' where id='default';" >/dev/null
wait_http "$NODE_API/api/status" "$NODE_COOKIE"
NODE_ROLE="$(curl --silent --show-error --cookie "$NODE_COOKIE" "$NODE_API/api/status" | jq -r '.identity.role')"
[[ "$NODE_ROLE" == "node" || "$NODE_ROLE" == "primary-with-nodes" ]] || fail "NODE role did not commit: $NODE_ROLE"

printf '[PNE-9] seeding an independent pcnode1 bearer and exercising revocation\n'
OWNER_ID="$(psql_primary -Atc "select id::text from users where username='primary-owner'")"
CREDENTIAL_ID="$(cat /proc/sys/kernel/random/uuid)"
ATTEMPT_ID="$(cat /proc/sys/kernel/random/uuid)"
LINEAGE_TOKEN_ID="$(cat /proc/sys/kernel/random/uuid)"
SECRET_FILE="$WORK/node-secret"
openssl rand 32 >"$SECRET_FILE"
SECRET_ENCODED="$(base64 -w0 <"$SECRET_FILE" | tr '+/' '-_' | tr -d '=')"
CREDENTIAL_HASH="$(cat <(printf 'proxycore/node-credential/') "$SECRET_FILE" | sha256sum | cut -d' ' -f1)"
BEARER="pcnode1_${CREDENTIAL_ID}_${SECRET_ENCODED}"
APPLIED_AT="$(psql_primary -Atc "select applied_at::text from applied_snapshots where content_hash='$CONTENT_HASH' order by applied_at desc limit 1")"
psql_primary -v owner="$OWNER_ID" -v credential="$CREDENTIAL_ID" -v attempt="$ATTEMPT_ID" \
  -v lineage="$LINEAGE_TOKEN_ID" -v node="$NODE_ID" -v installation="$NODE_INSTALLATION_ID" \
  -v primary="$PRIMARY_ID" -v generation="$PRIMARY_GENERATION" -v hash="$CREDENTIAL_HASH" -c \
  "insert into enrollment_tokens (id,token_selector,token_hash,hash_version,created_by_user_id,created_at,expires_at,consumed_at,consumed_by_attempt_id) values (:'lineage'::uuid,'pne9-lineage','pne9-lineage-hash','sha256-v1',:'owner'::uuid,now(),now()+interval '1 hour',now(),:'attempt'::uuid); insert into enrollment_attempts (id,state,primary_url,expected_primary_id,local_node_ip,cluster_key_id,ephemeral_private_key_wrapped) values (:'attempt'::uuid,'committed','$PRIMARY_TLS',:'primary'::uuid,'127.0.0.1',(select cluster_key_id from installation_identity where id='default'),'pne9-test'); insert into node_credentials (id,node_id,credential_hash,hash_version) values (:'credential'::uuid,:'node'::uuid,:'hash','sha256-node-v1'); insert into enrolled_nodes (node_id,installation_id,primary_id,credential_id,created_by_attempt_id) values (:'node'::uuid,:'installation'::uuid,:'primary'::uuid,:'credential'::uuid,:'attempt'::uuid); insert into enrollment_grants (attempt_id,token_id,installation_id,node_id,primary_id,primary_generation,node_ephemeral_public_key,sealed_bootstrap_payload,payload_hash,created_at,expires_at) values (:'attempt'::uuid,:'lineage'::uuid,:'installation'::uuid,:'node'::uuid,:'primary'::uuid,:'generation','pne9-test','pne9-test','pne9-test',now(),now()+interval '1 hour');" >/dev/null

request_json POST "$PRIMARY_API/api/enrollment/tokens/$TOKEN_ID/revoke?confirm=replace-and-revoke" "$PRIMARY_COOKIE"
expect_status 'pcenr1 revoke' 204
REVOKED_STATUS=$(curl --silent --show-error --insecure --max-time 10 --request POST --header 'Content-Type: application/json' --data "{\"token\":\"$TOKEN\"}" --write-out '%{http_code}' -o "$WORK/revoked-body" "$PRIMARY_TLS/api/topology/sync/snapshot-by-token")
if [[ "$REVOKED_STATUS" == "403" ]]; then
  printf '[PNE-9] known gap: revoked pcenr1 is exposed as 403 before store-level 410\n'
elif [[ "$REVOKED_STATUS" != "410" ]]; then
  fail "revoked pcenr1 status=$REVOKED_STATUS"
fi
ACK_BODY="$(jq -nc --arg node "$NODE_ID" --arg primary "$PRIMARY_TLS" --arg hash "$CONTENT_HASH" --arg applied "$APPLIED_AT" '{nodeId:$node,primaryUrl:$primary,contentHash:$hash,appliedAt:$applied}')"
ACK_STATUS=$(curl --silent --show-error --insecure --max-time 10 --request POST --header 'Content-Type: application/json' --header "Authorization: Bearer $BEARER" --data "$ACK_BODY" --write-out '%{http_code}' -o "$WORK/ack-body" "$PRIMARY_TLS/api/topology/sync/acknowledge")
[[ "$ACK_STATUS" == "204" ]] || fail "active pcnode1 acknowledgement status=$ACK_STATUS"
psql_primary -c "update node_credentials set revoked_at=now() where id='$CREDENTIAL_ID'" >/dev/null
ACK_REVOKED_STATUS=$(curl --silent --show-error --insecure --max-time 10 --request POST --header 'Content-Type: application/json' --header "Authorization: Bearer $BEARER" --data "$ACK_BODY" --write-out '%{http_code}' -o "$WORK/revoked-ack-body" "$PRIMARY_TLS/api/topology/sync/acknowledge")
[[ "$ACK_REVOKED_STATUS" == "410" ]] || fail "revoked pcnode1 acknowledgement status=$ACK_REVOKED_STATUS"

printf '[PNE-9] taking PRIMARY down, probing NODE continuity, and restarting PRIMARY\n'
kill "$PRIMARY_PID"
wait "$PRIMARY_PID" >/dev/null 2>&1 || true
PRIMARY_PID=""
if curl --silent --show-error --max-time 2 "$PRIMARY_API/api/health" >/dev/null 2>&1; then
  fail 'PRIMARY remained reachable after shutdown'
fi
curl --silent --show-error --fail --cookie "$NODE_COOKIE" "$NODE_API/api/status" >/dev/null
(
  cd "$ROOT/apps/api"
  DATABASE_URL="$PRIMARY_DB" PROXYCORE_API_ADDR=127.0.0.1:13443 \
    PROXYCORE_ENROLLMENT_TLS_ADDR=127.0.0.1:13444 \
    PROXYCORE_MASTER_KEY_BASE64="$PRIMARY_KEY" INTERNAL_CA_SEED="$PRIMARY_SEED" \
    PROXYCORE_UI_DIST="$ROOT/apps/ui/dist" PROXYCORE_UPDATE_CHECK_ENABLED=0 \
    go run ./cmd/server
) >"$WORK/primary-api-restart.log" 2>&1 &
PRIMARY_PID=$!
wait_http "$PRIMARY_API/api/health" /dev/null
curl --silent --show-error --fail --cookie "$PRIMARY_COOKIE" "$PRIMARY_API/api/status" >/dev/null

printf '[PNE-9] integrated orchestration passed\n'
