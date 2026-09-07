#!/bin/sh
# updater-bootstrap.sh — ProxyCore updater self-replacement sidecar.
#
# Runs as a long-running container (root, via Docker socket) in the same
# compose project as the updater. It watches a shared volume for an atomic
# request file written by the updater after a successful update. When
# present, it claims the file (mv), builds or pulls the new image, and
# recreates the updater container — without killing itself first.
#
# Paths and env overrides (all overridable via compose environment):
#   PROXYCORE_BOOTSTRAP_REQUEST_FILE  — atomic request JSON on the shared volume.
#   PROXYCORE_BOOTSTRAP_WORK_DIR      — staging directory for mv-claim.
#   PROXYCORE_BOOTSTRAP_INTERVAL      — poll interval in seconds (default 5).
#   PROXYCORE_BOOTSTRAP_RETRIES        — restart retry count (default 3).
#   PROXYCORE_BOOTSTRAP_MODE           — build local image or pull registry image.
#                                        Defaults to pull.
#   PROXYCORE_BOOTSTRAP_UPDATER_IMAGE  — resolved updater image reference.
#                                        A present :local image skips pull in pull mode.
#   PROXYCORE_COMPOSE_FILE             — compose.yaml path (default /app/compose.yaml).
#   PROXYCORE_COMPOSE_PROJECT          — compose project name.
#   PROXYCORE_COMPOSE_ENV_FILE         — .env file for --env-file.
#
# The bootstrap process:
#   1. Poll shared volume for the request JSON.
#   2. Atomically claim it by moving it into a private working directory.
#   3. Run docker compose build updater or docker compose pull updater.
#   4. Run docker compose up -d --no-deps --force-recreate --no-build updater
#      (replaces the running updater container with the newly-built/pulled image).
#   5. On success: delete the consumed request file.
#   6. On failure: restore the request file so the next cycle can retry
#      (bounded by PROXYCORE_BOOTSTRAP_RETRIES).
#
# Safe with: sh -n (no bashisms), paths and env overrides, Docker socket
# mount, and composable without modifying the updater's own lifecycle.

set -e

# --- Config with defaults (all overridable via environment) ---
REQUEST_FILE="${PROXYCORE_BOOTSTRAP_REQUEST_FILE:-/bootstrap/request.json}"
WORK_DIR="${PROXYCORE_BOOTSTRAP_WORK_DIR:-/bootstrap/work}"
INTERVAL="${PROXYCORE_BOOTSTRAP_INTERVAL:-5}"
RETRIES="${PROXYCORE_BOOTSTRAP_RETRIES:-3}"
BOOTSTRAP_MODE="${PROXYCORE_BOOTSTRAP_MODE:-pull}"
UPDATER_IMAGE="${PROXYCORE_BOOTSTRAP_UPDATER_IMAGE:-}"

COMPOSE_FILE="${PROXYCORE_COMPOSE_FILE:-/app/compose.yaml}"
COMPOSE_PROJECT="${PROXYCORE_COMPOSE_PROJECT:-proxycore}"
COMPOSE_ENV="${PROXYCORE_COMPOSE_ENV_FILE:-}"

UPSTREAM_LOG_PREFIX="[updater-bootstrap]"

case "$BOOTSTRAP_MODE" in
    build|pull) ;;
    *)
        echo "$UPSTREAM_LOG_PREFIX WARNING: unknown bootstrap mode '$BOOTSTRAP_MODE'; using pull" >&2
        BOOTSTRAP_MODE="pull"
        ;;
esac

# --- Guards ---
_dir=$(dirname "$REQUEST_FILE")
if [ ! -d "$_dir" ]; then
    echo "$UPSTREAM_LOG_PREFIX ERROR: request file directory does not exist: $_dir" >&2
    echo "$UPSTREAM_LOG_PREFIX ERROR: is the shared volume mounted at $_dir?" >&2
    exit 1
fi
unset _dir

if [ ! -S "/var/run/docker.sock" ]; then
    echo "$UPSTREAM_LOG_PREFIX ERROR: Docker socket not available at /var/run/docker.sock" >&2
    exit 1
fi

# Staging directory for atomic claim via mv.
mkdir -p "$WORK_DIR"

echo "$UPSTREAM_LOG_PREFIX started (request=$REQUEST_FILE interval=${INTERVAL}s retries=$RETRIES project=$COMPOSE_PROJECT mode=$BOOTSTRAP_MODE image=${UPDATER_IMAGE:-auto})"
echo "$UPSTREAM_LOG_PREFIX watching for bootstrap requests..."

# --- Run docker compose: builds base flags then appends caller's args ---
# Base flags are emitted by _compose_base; piped via a temp file so POSIX sh
# (dash) can read them without process substitution (<(...) is bash-only).
# Output (stdout+stderr) goes to stdout; exit code goes to stderr as __run_ec:N.
run_compose() {
    # Build the base flags into a temp file (POSIX-safe).
    _fifo=$(mktemp)
    _compose_base > "$_fifo"
    _cb=""
    while IFS= read -r _tok; do
        _cb="$_cb $(printf '%s' "$_tok" | sed "s/'/'\\\\''/g")"
    done < "$_fifo"
    rm -f "$_fifo"
    # Subshell so the outer `set -e` does not exit on non-zero compose exit.
    ( set +e
      # shellcheck disable=SC2086
      eval docker compose $_cb "$@" 2>&1
      echo "__run_ec:$?" >&2
    ) 2>&1
}

# Emit base flags to stdout, one per line.
_compose_base() {
    echo "-f"; echo "$COMPOSE_FILE"
    echo "-p"; echo "$COMPOSE_PROJECT"
    if [ -n "$COMPOSE_ENV" ]; then
        echo "--env-file"; echo "$COMPOSE_ENV"
    fi
}

# --- Main polling loop ---
while true; do
    # Only attempt to claim if the request file exists and is non-empty.
    if [ -s "$REQUEST_FILE" ]; then
        _stamp=$(date +%s)
        _claim="${WORK_DIR}/claimed-${_stamp}-$$.json"
        _claimed=""

        # Atomic claim: move the shared request to our private working directory.
        # mv is atomic on the same filesystem (WORK_DIR is on the shared volume).
        if mv "$REQUEST_FILE" "$_claim" 2>/dev/null; then
            _claimed="yes"
            echo "$UPSTREAM_LOG_PREFIX request claimed: $_claim"
            echo "$UPSTREAM_LOG_PREFIX contents: $(cat "$_claim")"
        fi
        unset _stamp

        if [ "$_claimed" = "yes" ]; then
            _attempt=0
            _success=""

            while [ -z "$_success" ] && [ "$_attempt" -lt "$RETRIES" ]; do
                _attempt=$((_attempt + 1))
                echo "$UPSTREAM_LOG_PREFIX $BOOTSTRAP_MODE attempt $_attempt/$RETRIES..."

                # Local images are rebuilt from the checked-out source. Remote
                # images are pulled; an existing :local image may be reused only
                # when pull mode is explicitly selected.
                _pout=""
                _perr=0
                if [ "$BOOTSTRAP_MODE" = "build" ]; then
                    _pout=$(run_compose build updater)
                    _perr=$(echo "$_pout" | grep '^__run_ec:' | sed 's/__run_ec://')
                    _pout=$(echo "$_pout" | sed '/^__run_ec:/d')
                else
                    case "$UPDATER_IMAGE" in
                        *:local)
                            if docker image inspect "$UPDATER_IMAGE" >/dev/null 2>&1; then
                                _pout="using existing local image $UPDATER_IMAGE"
                                echo "$UPSTREAM_LOG_PREFIX pull skipped: $_pout"
                            else
                                _pout=$(run_compose pull updater)
                                _perr=$(echo "$_pout" | grep '^__run_ec:' | sed 's/__run_ec://')
                                _pout=$(echo "$_pout" | sed '/^__run_ec:/d')
                            fi
                            ;;
                        *)
                            _pout=$(run_compose pull updater)
                            _perr=$(echo "$_pout" | grep '^__run_ec:' | sed 's/__run_ec://')
                            _pout=$(echo "$_pout" | sed '/^__run_ec:/d')
                            ;;
                    esac
                fi
                if [ "$_perr" -ne 0 ]; then
                    echo "$UPSTREAM_LOG_PREFIX $BOOTSTRAP_MODE failed (exit $_perr): $_pout"
                    # Build/pull failures stay fail-closed; restore the request
                    # below so a later apply can retry it.
                    break
                fi
                echo "$UPSTREAM_LOG_PREFIX $BOOTSTRAP_MODE output: $_pout"

                echo "$UPSTREAM_LOG_PREFIX recreate attempt $_attempt/$RETRIES..."
                # Recreate the updater container from the newly-built/pulled image.
                # --no-deps   skips postgres / api / etc.
                # --force-recreate ensures the container is replaced even if the
                #                  image tag is unchanged (defensive).
                # --no-build  skips an implicit second build.
                _uout=$(run_compose up -d --no-deps --force-recreate --no-build updater)
                _uperr=$(echo "$_uout" | grep '^__run_ec:' | sed 's/__run_ec://')
                _uout=$(echo "$_uout" | sed '/^__run_ec:/d')
                if [ "$_uperr" -ne 0 ]; then
                    echo "$UPSTREAM_LOG_PREFIX recreate failed (exit $_uperr): $_uout"
                    echo "$UPSTREAM_LOG_PREFIX recreate attempt $_attempt/$RETRIES failed; will retry after sleep"
                    sleep "$INTERVAL"
                    continue
                fi
                echo "$UPSTREAM_LOG_PREFIX recreate output: $_uout"

                _success="yes"
                echo "$UPSTREAM_LOG_PREFIX updater container replaced successfully"
            done

            if [ "$_success" = "yes" ]; then
                echo "$UPSTREAM_LOG_PREFIX removing consumed request: $_claim"
                rm -f "$_claim"
            else
                # Restore the request so the next poll cycle can retry.
                # Do NOT retry indefinitely — this guards against a corrupted
                # request file causing a tight loop.
                echo "$UPSTREAM_LOG_PREFIX WARNING: all restart attempts exhausted; restoring request for next cycle"
                if mv "$_claim" "$REQUEST_FILE" 2>/dev/null; then
                    echo "$UPSTREAM_LOG_PREFIX request restored: $REQUEST_FILE"
                else
                    echo "$UPSTREAM_LOG_PREFIX ERROR: failed to restore request file; manual intervention may be required" >&2
                fi
                # Sleep to avoid tight loop on persistent failure.
                sleep "$INTERVAL"
            fi
            unset _attempt _success _pout _perr _uout _uperr
        fi
        unset _claim _claimed
    fi

    sleep "$INTERVAL"
done
