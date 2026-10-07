#!/bin/sh
# ProxyCore legacy PostgreSQL -> SQLite conversion (offline, one way)
#
# Usage (from the install directory, e.g. /opt/proxycore):
#   sh scripts/convert-postgres.sh
#
# What it does:
#   1. Refuses to run while the api or worker (or any container) still uses the
#      legacy volume, so the snapshot is consistent.
#   2. Starts a TEMPORARY PostgreSQL container on the legacy
#      <project>_postgres-data volume, attached only to a private --internal
#      Docker network (nothing is published on the host).
#   3. Runs the proxycore-convert binary shipped in the api image against it
#      and writes /var/lib/proxycore-db/proxycore.db on the sqlite-data volume.
#   4. Verifies the proxycore.db.converted.json success marker.
#   5. ALWAYS removes the temporary container and network. It never removes the
#      legacy volume and never prints credentials or the PostgreSQL URL.
#
# The script is idempotent: when proxycore.db and its marker already exist it
# exits 0 without touching anything.
#
# Optional environment:
#   COMPOSE_PROJECT_NAME        Compose project (default: proxycore)
#   POSTGRES_USER/POSTGRES_DB/POSTGRES_PASSWORD
#                               Legacy credentials. Default: values from .env
#                               (read as data, never sourced), then the old
#                               compose defaults (proxycore / proxycore / change-me).
#   PROXYCORE_LEGACY_PG_IMAGE   Image used for the temporary server
#                               (default: postgres:17-alpine, the old image)
#
# Why the volume is mounted read-write: PostgreSQL cannot start on a read-only
# data directory (it writes postmaster.pid and may replay WAL after an unclean
# stop). The converter itself only reads, inside one REPEATABLE READ READ ONLY
# transaction. Take a volume backup first (see docs/runbooks/upgrade-from-postgres.md).

set -eu

# Byte-wise string handling for the URL encoder below.
LC_ALL=C
export LC_ALL

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
PROJECT="${COMPOSE_PROJECT_NAME:-proxycore}"
LEGACY_VOLUME="${PROJECT}_postgres-data"
PG_IMAGE="${PROXYCORE_LEGACY_PG_IMAGE:-postgres:17-alpine}"
PG_CONTAINER=proxycore-legacy-pg
PG_NETWORK=proxycore-legacy-pg-net
PG_ALIAS=legacy-pg
SQLITE_DB=/var/lib/proxycore-db/proxycore.db
MARKER="${SQLITE_DB}.converted.json"

OVERRIDE_FILE=''
PG_STARTED=0
NETWORK_CREATED=0

log() {
  printf '==> %s\n' "$*"
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

cleanup() {
  status=$?
  trap - EXIT HUP INT TERM
  if [ "$PG_STARTED" = 1 ]; then
    # No -v: never remove volumes. The legacy volume is not removed either way.
    docker rm -f "$PG_CONTAINER" >/dev/null 2>&1 || :
  fi
  if [ "$NETWORK_CREATED" = 1 ]; then
    docker network rm "$PG_NETWORK" >/dev/null 2>&1 || :
  fi
  if [ -n "$OVERRIDE_FILE" ]; then
    rm -f "$OVERRIDE_FILE" 2>/dev/null || :
  fi
  exit "$status"
}

# Read KEY from .env as data (first assignment, CR stripped, one pair of
# surrounding quotes removed). The file is never sourced or evaluated.
env_value() {
  [ -f "$ROOT/.env" ] || return 0
  tr -d '\r' < "$ROOT/.env" \
    | sed -n "s/^[[:space:]]*$1[[:space:]]*=[[:space:]]*//p" \
    | sed -n '1p' \
    | sed -e 's/^"\(.*\)"$/\1/' -e "s/^'\\(.*\\)'\$/\\1/"
}

urlencode() {
  enc_in=$1
  enc_out=''
  while [ -n "$enc_in" ]; do
    enc_rest=${enc_in#?}
    enc_char=${enc_in%"$enc_rest"}
    case "$enc_char" in
      [A-Za-z0-9._~-]) enc_out="${enc_out}${enc_char}" ;;
      *) enc_out="${enc_out}$(printf '%%%02X' "'$enc_char")" ;;
    esac
    enc_in=$enc_rest
  done
  printf '%s' "$enc_out"
}

compose() {
  docker compose --project-directory "$ROOT" -f "$ROOT/compose.yaml" "$@"
}

# Same as compose, plus an override that attaches the api service to the
# private network where the temporary PostgreSQL lives.
compose_convert() {
  docker compose --project-directory "$ROOT" -f "$ROOT/compose.yaml" -f "$OVERRIDE_FILE" "$@"
}

# Run `test <flag> <path>` inside the api image on the sqlite-data volume.
sqlite_has() {
  compose run --rm --no-deps -T --entrypoint test api "$1" "$2" >/dev/null 2>&1
}

refuse_if_running() {
  for service in api worker; do
    running=$(docker ps -q \
      --filter "label=com.docker.compose.project=${PROJECT}" \
      --filter "label=com.docker.compose.service=${service}")
    if [ -n "$running" ]; then
      die "the ${service} container is running; stop api and worker first so the snapshot is consistent: docker compose stop api worker"
    fi
  done
  users=$(docker ps -q --filter "volume=${LEGACY_VOLUME}")
  if [ -n "$users" ]; then
    die "a container is still using ${LEGACY_VOLUME} (a second PostgreSQL on the same data directory could corrupt it). Stop it first: docker stop \$(docker ps -q --filter volume=${LEGACY_VOLUME})"
  fi
}

wait_postgres() {
  pg_user=$1
  pg_db=$2
  attempts=90
  while [ "$attempts" -gt 0 ]; do
    if docker exec "$PG_CONTAINER" pg_isready -U "$pg_user" -d "$pg_db" >/dev/null 2>&1 \
      && docker exec "$PG_CONTAINER" psql -U "$pg_user" -d "$pg_db" -Atc 'select 1' >/dev/null 2>&1; then
      return 0
    fi
    attempts=$((attempts - 1))
    sleep 1
  done
  die "temporary PostgreSQL did not become ready (check: docker logs ${PG_CONTAINER})"
}

explain_exit_code() {
  case "$1" in
    1) printf 'conversion failed validation or fidelity checks; nothing was published (the converter removed its temporary files)\n' ;;
    2) printf 'converter usage error, or the output database already exists\n' ;;
    3) printf 'PostgreSQL unreachable, or a schema/data shape the converter does not know\n' ;;
    *) printf 'unexpected converter exit code\n' ;;
  esac
}

main() {
  need_cmd docker
  need_cmd tr
  need_cmd sed
  docker compose version >/dev/null 2>&1 || die "Docker Compose v2 is required (docker compose)"
  docker info >/dev/null 2>&1 || die "Docker daemon is not reachable (is the user in the docker group?)"

  if ! docker volume inspect "$LEGACY_VOLUME" >/dev/null 2>&1; then
    log "No legacy PostgreSQL volume (${LEGACY_VOLUME}); nothing to convert"
    return 0
  fi

  if sqlite_has -f "$SQLITE_DB"; then
    if sqlite_has -f "$MARKER"; then
      log "Already converted: ${SQLITE_DB} and its marker exist; nothing to do"
      return 0
    fi
    die "${SQLITE_DB} exists but has no ${MARKER}. It was not produced by this converter, so it is not overwritten and not trusted as a conversion. If it was created by a fresh start of the new release, inspect it before deciding; the legacy volume was left untouched"
  fi

  refuse_if_running

  if [ -z "${PROXYCORE_MASTER_KEY_BASE64:-}" ] && [ -z "$(env_value PROXYCORE_MASTER_KEY_BASE64)" ]; then
    die "PROXYCORE_MASTER_KEY_BASE64 is not set in ${ROOT}/.env or the environment; the converter needs the key the installation already uses"
  fi

  if [ -n "$(docker ps -aq --filter "name=^/${PG_CONTAINER}\$")" ]; then
    die "a stale ${PG_CONTAINER} container exists; remove it first: docker rm -f ${PG_CONTAINER}"
  fi

  pg_user="${POSTGRES_USER:-$(env_value POSTGRES_USER)}"
  pg_db="${POSTGRES_DB:-$(env_value POSTGRES_DB)}"
  pg_password="${POSTGRES_PASSWORD:-$(env_value POSTGRES_PASSWORD)}"
  pg_user="${pg_user:-proxycore}"
  pg_db="${pg_db:-proxycore}"
  pg_password="${pg_password:-change-me}"
  case "$pg_user$pg_db" in
    *[!A-Za-z0-9_.-]*) die "POSTGRES_USER/POSTGRES_DB contain unsupported characters" ;;
  esac

  trap cleanup EXIT
  trap 'exit 130' INT
  trap 'exit 143' HUP TERM

  OVERRIDE_FILE=$(mktemp "${TMPDIR:-/tmp}/proxycore-convert-override.XXXXXX")
  cat > "$OVERRIDE_FILE" <<EOF
services:
  api:
    networks: [legacy-pg]
networks:
  legacy-pg:
    external: true
    name: ${PG_NETWORK}
EOF

  log "Creating private network ${PG_NETWORK}"
  docker network create --internal "$PG_NETWORK" >/dev/null || die "unable to create network ${PG_NETWORK}"
  NETWORK_CREATED=1

  log "Starting temporary PostgreSQL on ${LEGACY_VOLUME} (not published on the host)"
  PG_STARTED=1
  docker run -d --rm --name "$PG_CONTAINER" \
    --network "$PG_NETWORK" --network-alias "$PG_ALIAS" \
    -v "${LEGACY_VOLUME}:/var/lib/postgresql/data" \
    "$PG_IMAGE" >/dev/null || die "unable to start the temporary PostgreSQL container"
  wait_postgres "$pg_user" "$pg_db"

  PROXYCORE_CONVERT_PG_URL="postgres://$(urlencode "$pg_user"):$(urlencode "$pg_password")@${PG_ALIAS}:5432/$(urlencode "$pg_db")?sslmode=disable"
  export PROXYCORE_CONVERT_PG_URL
  pg_password=''

  log "Converting to ${SQLITE_DB} (this can take a while)"
  convert_rc=0
  compose_convert run --rm --no-deps -T -e PROXYCORE_CONVERT_PG_URL \
    --entrypoint proxycore-convert api --out "$SQLITE_DB" || convert_rc=$?
  unset PROXYCORE_CONVERT_PG_URL

  if [ "$convert_rc" -ne 0 ]; then
    printf 'error: proxycore-convert exited with %s: %s\n' "$convert_rc" "$(explain_exit_code "$convert_rc")" >&2
    printf 'The legacy PostgreSQL volume was not modified logically and was not removed.\n' >&2
    exit "$convert_rc"
  fi

  sqlite_has -f "$SQLITE_DB" || die "converter reported success but ${SQLITE_DB} is missing"
  sqlite_has -f "$MARKER" || die "converter reported success but ${MARKER} is missing; do not use the database"

  log "Conversion validated (${MARKER} written)"
  printf '\n'
  printf 'Next: review the warnings printed above, then start the new release:\n'
  printf '  sh scripts/install.sh   (or: docker compose up -d --build)\n'
  printf 'The legacy volume %s was kept for rollback. Remove it only after you are\n' "$LEGACY_VOLUME"
  printf 'sure you will not go back (see docs/runbooks/upgrade-from-postgres.md).\n'
}

if [ "${PROXYCORE_CONVERT_NO_MAIN:-0}" != "1" ]; then
  main "$@"
fi
