#!/bin/sh
# ProxyCore Nginx entrypoint.
# Restore the last promoted configuration from the persistent candidates
# volume before handing off to the official Nginx entrypoint.

set -eu

CANDIDATE_ROOT="${PROXYCORE_NGINX_CANDIDATE_ROOT:-/var/lib/proxycore/candidates}"
CANDIDATE_ROOT="${CANDIDATE_ROOT%/}"
STABLE_CONF="${PROXYCORE_NGINX_LIVE_CONFIG:-${CANDIDATE_ROOT}/nginx-live.conf}"
NGINX_CONF_DEST="/etc/nginx/nginx.conf"

log() {
    echo "[proxycore-entrypoint] $*" >&2
}

validate_config() {
    _source="$1"
    nginx -t -c "$_source" >/dev/null 2>&1
}

install_config() {
    _source="$1"
    _label="$2"
    if ! validate_config "$_source"; then
        log "[$_label] validation failed; skipping"
        return 1
    fi
    cp "$_source" "$NGINX_CONF_DEST"
    log "[$_label] installed as $NGINX_CONF_DEST"
    return 0
}

find_newest_legacy_candidate() {
    _root="$1"
    _paths_file=$(mktemp)
    _newest_time=0
    _newest_path=""

    find "$_root" -type f -name "nginx.conf" -print 2>/dev/null >"$_paths_file" || true
    while IFS= read -r _path; do
        case "$_path" in
            */nginx/nginx.conf)
                _mtime=$(stat -c %Y "$_path" 2>/dev/null || echo 0)
                if [ "$_mtime" -gt "$_newest_time" ]; then
                    _newest_time="$_mtime"
                    _newest_path="$_path"
                fi
                ;;
        esac
    done <"$_paths_file"
    rm -f "$_paths_file"

    if [ -n "$_newest_path" ]; then
        printf '%s\n' "$_newest_path"
    fi
}

log "Starting ProxyCore Nginx entrypoint"
log "Candidates root: $CANDIDATE_ROOT"
log "Stable config:   $STABLE_CONF"

_installed=0
if [ -s "$STABLE_CONF" ]; then
    log "Stable live config found; validating..."
    if install_config "$STABLE_CONF" "stable"; then
        _installed=1
    else
        log "WARNING: stable config is invalid; trying legacy candidates"
    fi
else
    log "No stable live config; trying legacy candidates"
fi

if [ "$_installed" -eq 0 ]; then
    _legacy=$(find_newest_legacy_candidate "$CANDIDATE_ROOT")
    if [ -n "$_legacy" ] && install_config "$_legacy" "legacy"; then
        _installed=1
    fi
fi

if [ "$_installed" -eq 0 ]; then
    log "WARNING: no valid persistent Nginx config found; using baked default"
    log "WARNING: Apply a configuration to restore full functionality"
fi

log "Handing off to /docker-entrypoint.sh"
exec /docker-entrypoint.sh "$@"
