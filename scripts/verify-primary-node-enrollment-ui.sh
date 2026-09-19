#!/usr/bin/env bash
# Verify the UI boundary for PNE-9 without mutating application source files.
set -u -o pipefail

PNE9_UI_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PNE9_UI_FAILURES=0

pne9_ui_run() {
  local label=$1
  shift
  printf '\n[PNE-9 UI] %s\n' "$label"
  if "$@"; then
    printf '[PNE-9 UI] %s: PASS (exit=0)\n' "$label"
  else
    local status=$?
    printf '[PNE-9 UI] %s: FAIL (exit=%s)\n' "$label" "$status" >&2
    PNE9_UI_FAILURES=$((PNE9_UI_FAILURES + 1))
  fi
}

pne9_ui_typecheck() {
  (cd "$PNE9_UI_ROOT/apps/ui" && npx tsc --noEmit)
}

pne9_ui_build() {
  (cd "$PNE9_UI_ROOT/apps/ui" && npm run build)
}

pne9_ui_tests() {
  local output status test_line
  output=$(cd "$PNE9_UI_ROOT" && npx vitest run --reporter=dot 2>&1)
  status=$?
  printf '%s\n' "$output"
  test_line=$(printf '%s\n' "$output" | awk '/Tests[[:space:]]+[0-9]+ passed/{print; exit}')
  if [[ -n "$test_line" ]]; then
    printf '[PNE-9 UI] Vitest count: %s\n' "$test_line"
  else
    printf '[PNE-9 UI] Vitest count: unavailable\n' >&2
  fi
  return "$status"
}

pne9_ui_dev_smoke() {
  if [[ "${PNE9_UI_DEV_SMOKE:-0}" != "1" ]]; then
    printf '[PNE-9 UI] dev-server smoke: SKIP (set PNE9_UI_DEV_SMOKE=1 to enable)\n'
    return 0
  fi
  if ! command -v curl >/dev/null 2>&1; then
    printf '[PNE-9 UI] dev-server smoke: SKIP (curl is unavailable)\n'
    return 0
  fi

  local port=${PNE9_UI_DEV_PORT:-14173}
  local pid
  (cd "$PNE9_UI_ROOT/apps/ui" && exec npx vite --host 127.0.0.1 --port "$port") >/dev/null 2>&1 &
  pid=$!
  trap 'kill "$pid" >/dev/null 2>&1 || true; wait "$pid" >/dev/null 2>&1 || true' RETURN
  local ready=0
  local attempt
  for attempt in $(seq 1 50); do
    if curl --fail --silent --show-error "http://127.0.0.1:${port}/" >/dev/null 2>&1; then
      ready=1
      break
    fi
    sleep 0.2
  done
  kill "$pid" >/dev/null 2>&1 || true
  wait "$pid" >/dev/null 2>&1 || true
  trap - RETURN
  if [[ "$ready" != "1" ]]; then
    printf '[PNE-9 UI] dev-server smoke: FAIL (no HTTP 200)\n' >&2
    return 1
  fi
  printf '[PNE-9 UI] dev-server smoke: PASS (HTTP 200)\n'
}

verify_primary_node_enrollment_ui() {
  PNE9_UI_FAILURES=0
  pne9_ui_run "TypeScript no-emit" pne9_ui_typecheck
  pne9_ui_run "Vite production build" pne9_ui_build
  pne9_ui_run "Full Vitest suite" pne9_ui_tests
  pne9_ui_run "Optional dev-server HTTP smoke" pne9_ui_dev_smoke
  if [[ "$PNE9_UI_FAILURES" != "0" ]]; then
    printf '[PNE-9 UI] verification failed: %s step(s) failed\n' "$PNE9_UI_FAILURES" >&2
    return 1
  fi
  printf '[PNE-9 UI] verification passed\n'
  return 0
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  verify_primary_node_enrollment_ui
  exit $?
fi
