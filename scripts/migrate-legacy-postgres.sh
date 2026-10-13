#!/bin/sh
# ProxyCore migrate service: converts a legacy PostgreSQL installation to
# SQLite during a dashboard update, without a terminal.
#
# This is the entrypoint of the `migrate` service (infra/compose/Dockerfile.migrate).
# The previous release's updater runs `docker compose run --rm migrate` after it
# builds the new images and before it restarts the stack. Ordering:
#
#   0. Take the lock <data>/.migrate-legacy.lock (mkdir). A second run that
#      overlaps this one refuses here, so the two cannot clean each other's files.
#      A lock left by a killed run is reclaimed only when no other migrate
#      container of the project is running; then only proxycore.db.tmp* from
#      that run is removed (never a database or marker).
#   1. No legacy volume <project>_postgres-data      -> nothing to do, exit 0.
#   2. SQLite database and its marker both exist      -> already converted, exit 0.
#   3. Database without marker, or marker without it  -> refuse (fail closed).
#   3b. Partial output (proxycore.db.tmp, sidecars) without a database -> refuse;
#       an earlier conversion may still be running or was killed.
#   4. PROXYCORE_MASTER_KEY_BASE64 missing            -> refuse.
#   5. Legacy postgres:5432 not accepting connections,
#      or it rejects the POSTGRES_* credentials       -> refuse (fail closed).
#      Steps 4 and 5 run before anything is stopped.
#   6. Quiesce: stop the running api and worker containers of this project,
#      the only writers of the SQLite database, so the snapshot is consistent.
#      The legacy postgres keeps running; the converter only reads it.
#   7. Run proxycore-convert against postgres:5432 as uid 10001.
#   8. Set the owner and mode of proxycore.db and its marker (10001:10001, 0600).
#      api and worker stay stopped; the updater's restart step recreates them.
#
# If anything fails after step 6, the partial output is removed and the stopped
# containers are started again, because the old updater does not restart them
# when migrate fails. An interrupt (SIGTERM/SIGHUP) takes the same path.
#
# Never prints credentials, the PostgreSQL URL or the master key. The converter's
# own report (tables, rows, warnings) is printed as-is.
#
# Environment (set by compose.yaml, all optional):
#   HOSTNAME                      Container id; used to read the compose project label.
#   PROXYCORE_COMPOSE_PROJECT     Fallback project name (default: proxycore).
#   POSTGRES_USER/POSTGRES_DB/POSTGRES_PASSWORD
#                                 Legacy credentials (defaults: proxycore / proxycore / change-me).
#   PROXYCORE_MASTER_KEY_BASE64   Required. The key the installation already uses.
#   PROXYCORE_MIGRATE_DATA_DIR    Test hook (default: /var/lib/proxycore-db).
#   PROXYCORE_CONVERT_BIN         Test hook (default: proxycore-convert).

set -eu

# Byte-wise string handling for the URL encoder below.
LC_ALL=C
export LC_ALL

DATA_DIR="${PROXYCORE_MIGRATE_DATA_DIR:-/var/lib/proxycore-db}"
SQLITE_DB="${DATA_DIR}/proxycore.db"
MARKER="${SQLITE_DB}.converted.json"
CONVERT_BIN="${PROXYCORE_CONVERT_BIN:-proxycore-convert}"
RUN_AS="10001:10001"
PG_HOST=postgres
PG_PORT=5432
LOCK_DIR="${DATA_DIR}/.migrate-legacy.lock"

PROJECT=''
LEGACY_VOLUME=''
LOCK_HELD=0
CONVERT_STARTED=0
CONVERT_PID=''
CONVERT_OK=0
QUIESCED=''

log() {
  printf '==> %s\n' "$*"
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

# Remove output this run created. Only called after the pre-checks proved that
# no database or marker existed before the converter started.
remove_partial_output() {
  rm -f "$SQLITE_DB" "$SQLITE_DB-wal" "$SQLITE_DB-shm" "$SQLITE_DB-journal" \
    "$SQLITE_DB.tmp" "$SQLITE_DB.tmp-wal" "$SQLITE_DB.tmp-shm" "$SQLITE_DB.tmp-journal" \
    "$MARKER" "$MARKER.tmp" 2>/dev/null || :
}

restart_quiesced() {
  [ -n "$QUIESCED" ] || return 0
  log "Starting the api and worker containers again"
  # Intentional word splitting: QUIESCED is a list of container ids.
  # shellcheck disable=SC2086
  if ! docker start $QUIESCED >/dev/null 2>&1; then
    printf 'warning: could not start every stopped container; check: docker ps -a\n' >&2
  fi
  QUIESCED=''
}

on_exit() {
  status=$?
  trap - EXIT HUP INT TERM
  if [ -n "$CONVERT_PID" ] && kill -0 "$CONVERT_PID" 2>/dev/null; then
    kill -TERM "$CONVERT_PID" 2>/dev/null || :
    wait "$CONVERT_PID" 2>/dev/null || :
  fi
  if [ "$CONVERT_OK" != 1 ]; then
    if [ "$CONVERT_STARTED" = 1 ]; then
      remove_partial_output
    fi
    restart_quiesced
  fi
  if [ "$LOCK_HELD" = 1 ]; then
    rmdir "$LOCK_DIR" 2>/dev/null || :
  fi
  exit "$status"
}

trap on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' HUP TERM

# Compose project of this container. Read from the container's own labels so
# that `docker compose -p <name> run migrate` converts the right volume even
# though compose does not export -p to the interpolation of compose.yaml.
detect_project() {
  if [ -n "${HOSTNAME:-}" ]; then
    label=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project"}}' "$HOSTNAME" 2>/dev/null || :)
    case "$label" in
      '' | '<no value>') ;;
      *)
        printf '%s\n' "$label"
        return 0
        ;;
    esac
  fi
  printf '%s\n' "${PROXYCORE_COMPOSE_PROJECT:-proxycore}"
}

# Is another migrate container of this project running? Returns 0 when one is,
# 1 when none is, 2 when Docker could not be asked (fail closed). The current
# container is excluded by its id (HOSTNAME is its short id).
other_migrate_running() {
  if ! ids=$(docker ps -q --no-trunc \
    --filter "label=com.docker.compose.project=${PROJECT}" \
    --filter "label=com.docker.compose.service=migrate" 2>/dev/null); then
    return 2
  fi
  for id in $ids; do
    if [ -n "${HOSTNAME:-}" ]; then
      case "$id" in
        "$HOSTNAME"*) continue ;;
      esac
    fi
    return 0
  done
  return 1
}

# The lock exists. Reclaim it only when no other migrate container runs: then
# the run that created it was killed and its lock is stale. The lock is moved
# away atomically (only one run can win), a new lock is created before anything
# else is touched, and only the conversion artifacts of that killed run are
# removed. A database or marker is never removed here; main() decides about them.
reclaim_stale_lock() {
  other_migrate_running && rc=0 || rc=$?
  case "$rc" in
    0) die "another migrate run holds ${LOCK_DIR} and is still running (check: docker ps --filter name=migrate). Nothing was changed" ;;
    2) die "could not list the migrate containers of project ${PROJECT}; the lock was kept. Nothing was changed" ;;
  esac

  stale_claim="${LOCK_DIR}.stale.$$"
  mv "$LOCK_DIR" "$stale_claim" 2>/dev/null \
    || die "${LOCK_DIR} changed while it was checked; another migrate run may have started. Nothing was changed"
  rmdir "$stale_claim" 2>/dev/null || :
  mkdir "$LOCK_DIR" 2>/dev/null \
    || die "another migrate run took ${LOCK_DIR} while this run reclaimed it. Nothing was changed"
  LOCK_HELD=1
  log "Reclaimed the stale migrate lock left by a killed run"

  if [ -e "$SQLITE_DB" ] || [ -e "$MARKER" ]; then
    return 0
  fi
  for artifact in "$SQLITE_DB".tmp*; do
    [ -e "$artifact" ] || continue
    log "Removing partial conversion output of the killed run: ${artifact}"
    rm -f -- "$artifact"
  done
}

# One migrate run at a time. mkdir is atomic. A lock whose run was killed is
# reclaimed by reclaim_stale_lock; a lock held by a running migrate is refused.
acquire_lock() {
  if mkdir "$LOCK_DIR" 2>/dev/null; then
    LOCK_HELD=1
    return 0
  fi
  reclaim_stale_lock
}

# Refuse when conversion output exists but no database does. The files are not
# ours to delete: an earlier or concurrent conversion may still be writing them.
refuse_partial_output() {
  for leftover in "$SQLITE_DB.tmp" "$SQLITE_DB.tmp-wal" "$SQLITE_DB.tmp-shm" "$SQLITE_DB.tmp-journal" \
    "$MARKER.tmp" "$SQLITE_DB-wal" "$SQLITE_DB-shm" "$SQLITE_DB-journal"; do
    if [ -e "$leftover" ]; then
      die "${leftover} exists but ${SQLITE_DB} does not: an earlier conversion may still be running or was killed. Nothing was changed; inspect it before removing it"
    fi
  done
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

explain_exit_code() {
  case "$1" in
    1) printf 'conversion failed validation or fidelity checks; nothing was published\n' ;;
    2) printf 'converter usage error, or the output database already exists\n' ;;
    3) printf 'PostgreSQL unreachable, or a schema/data shape the converter does not know\n' ;;
    *) printf 'unexpected converter exit code\n' ;;
  esac
}

# Refuse unless the legacy PostgreSQL accepts the configured credentials. Runs
# before anything is stopped. The password only reaches psql through PGPASSWORD.
check_legacy_postgres() {
  if ! pg_isready -h "$PG_HOST" -p "$PG_PORT" -t 5 >/dev/null 2>&1; then
    die "legacy PostgreSQL volume ${LEGACY_VOLUME} exists but ${PG_HOST}:${PG_PORT} is not accepting connections. Nothing was stopped. Start the legacy postgres container of this project and run the update again, or convert manually with scripts/convert-postgres.sh (see docs/runbooks/upgrade-from-postgres.md)"
  fi
  if ! PGPASSWORD="$PG_PASSWORD" PGSSLMODE=disable psql -h "$PG_HOST" -p "$PG_PORT" \
    -U "$PG_USER" -d "$PG_DB" -v ON_ERROR_STOP=1 -Atc 'select 1' >/dev/null 2>&1; then
    die "legacy PostgreSQL at ${PG_HOST}:${PG_PORT} rejected the POSTGRES_USER/POSTGRES_DB/POSTGRES_PASSWORD values from .env. Nothing was stopped. Fix the values, or convert manually with scripts/convert-postgres.sh (see docs/runbooks/upgrade-from-postgres.md)"
  fi
}

# Stop the running api and worker containers of this project and remember
# their ids so they can be started again. Only these two services write SQLite.
quiesce_writers() {
  for service in api worker; do
    ids=$(docker ps -q \
      --filter "label=com.docker.compose.project=${PROJECT}" \
      --filter "label=com.docker.compose.service=${service}")
    for id in $ids; do
      QUIESCED="${QUIESCED:+$QUIESCED }$id"
    done
  done
  if [ -z "$QUIESCED" ]; then
    log "api and worker are not running; nothing to stop"
    return 0
  fi
  log "Stopping api and worker so the snapshot is consistent"
  # shellcheck disable=SC2086
  docker stop $QUIESCED >/dev/null || die "could not stop api and worker; nothing was converted"
}

main() {
  docker info >/dev/null 2>&1 || die "the Docker daemon is not reachable from the migrate container; the legacy PostgreSQL data was not checked, nothing was changed"

  PROJECT=$(detect_project)
  LEGACY_VOLUME="${PROJECT}_postgres-data"

  acquire_lock

  if ! docker volume inspect "$LEGACY_VOLUME" >/dev/null 2>&1; then
    log "No legacy PostgreSQL data (${LEGACY_VOLUME}); nothing to convert"
    return 0
  fi

  if [ -f "$SQLITE_DB" ]; then
    if [ -f "$MARKER" ]; then
      log "Already converted: ${SQLITE_DB} and its marker exist; nothing to do"
      return 0
    fi
    die "${SQLITE_DB} exists but has no ${MARKER}. It was not produced by the converter, so it is not overwritten and not trusted. Inspect it before deciding; the legacy volume was left untouched"
  fi
  if [ -f "$MARKER" ]; then
    die "${MARKER} exists without ${SQLITE_DB}. Remove the stray marker after inspection; the legacy volume was left untouched"
  fi
  refuse_partial_output

  if [ -z "${PROXYCORE_MASTER_KEY_BASE64:-}" ]; then
    die "PROXYCORE_MASTER_KEY_BASE64 is not set in the install .env; the converter needs the key the installation already uses. Nothing was stopped"
  fi

  PG_USER="${POSTGRES_USER:-proxycore}"
  PG_DB="${POSTGRES_DB:-proxycore}"
  PG_PASSWORD="${POSTGRES_PASSWORD:-change-me}"
  check_legacy_postgres

  quiesce_writers

  PROXYCORE_CONVERT_PG_URL="postgres://$(urlencode "$PG_USER"):$(urlencode "$PG_PASSWORD")@${PG_HOST}:${PG_PORT}/$(urlencode "$PG_DB")?sslmode=disable"
  export PROXYCORE_CONVERT_PG_URL
  PG_PASSWORD=''

  log "Converting legacy PostgreSQL to ${SQLITE_DB} (this can take a while)"
  CONVERT_STARTED=1
  # Background + wait so an interrupt can stop the converter before cleanup.
  su-exec "$RUN_AS" "$CONVERT_BIN" --out "$SQLITE_DB" &
  CONVERT_PID=$!
  convert_rc=0
  wait "$CONVERT_PID" || convert_rc=$?
  CONVERT_PID=''
  unset PROXYCORE_CONVERT_PG_URL

  if [ "$convert_rc" -ne 0 ]; then
    printf 'error: proxycore-convert exited with %s: %s\n' "$convert_rc" "$(explain_exit_code "$convert_rc")" >&2
    printf 'The legacy PostgreSQL volume was not modified and was not removed.\n' >&2
    exit "$convert_rc"
  fi

  [ -f "$SQLITE_DB" ] && [ -f "$MARKER" ] \
    || die "converter reported success but ${SQLITE_DB} or ${MARKER} is missing; do not use the database"

  chown "$RUN_AS" "$SQLITE_DB"
  chown "$RUN_AS" "$MARKER"
  chmod 600 "$SQLITE_DB" "$MARKER"
  CONVERT_OK=1

  log "Conversion validated (${MARKER} written)"
  printf 'api and worker stay stopped; the updater starts them with the new release.\n'
}

main "$@"
