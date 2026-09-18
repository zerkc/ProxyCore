#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
. "$ROOT/scripts/install_env.sh"
source_installer() {
  PROXYCORE_INSTALL_NO_MAIN=1 . "$ROOT/scripts/install.sh"
}

run_loader() {
  input=$1
  printf '%s' "$input" | (
    unset WEB_PORT ENROLLMENT_PORT DNS_PORT POSTGRES_USER POSTGRES_DB
    source_installer
    load_env_values -
    apply_env_file_values
    printf '%s|%s|%s|%s|%s\n' \
      "$WEB_PORT" "$ENROLLMENT_PORT" "$DNS_PORT" "$POSTGRES_USER" "$POSTGRES_DB"
  )
}

assert_rejected() {
  label=$1
  input=$2
  output=''
  if output=$(run_loader "$input" 2>&1); then
    printf '%s was accepted\n' "$label" >&2
    exit 1
  fi
  case "$output" in
    *ENROLLMENT_SIDE_EFFECT*)
      printf '%s executed payload output\n' "$label" >&2
      exit 1
      ;;
  esac
}

grammar_env=$(printf '%b' ' \t# comment\r\n WEB_PORT \t = \t "3000"\r\n DNS_PORT = 53 # unquoted comment\r\n UNKNOWN=$(printf ENROLLMENT_SIDE_EFFECT >&2)\r\n')
single_quote_env="ENROLLMENT_PORT='3443'"
[ "$(run_loader "$grammar_env
$single_quote_env")" = '3000|3443|53|proxycore|proxycore' ] || {
  printf 'bounded dotenv grammar did not load valid CRLF data\n' >&2
  exit 1
}
quoted_comment_env=$(printf '%b' 'WEB_PORT="3000" # double quote\r\nENROLLMENT_PORT='\''3443'\'' # single quote\r\nDNS_PORT=53 # plain\r\n')
[ "$(run_loader "$quoted_comment_env")" = '3000|3443|53|proxycore|proxycore' ] || {
  printf 'quoted port comments were not accepted\n' >&2
  exit 1
}

unknown_opaque='not a dotenv assignment
UNKNOWN=`printf ENROLLMENT_SIDE_EFFECT >&2`'
[ "$(run_loader "$unknown_opaque")" = '3000|3443|53|proxycore|proxycore' ] || {
  printf 'unknown dotenv lines were not ignored opaquely\n' >&2
  exit 1
}
unknown_after_known='WEB_PORT=3000
UNKNOWN_PROJECT_SETTING'
[ "$(run_loader "$unknown_after_known")" = '3000|3443|53|proxycore|proxycore' ] || {
  printf 'unknown dotenv keys after known keys were not ignored\n' >&2
  exit 1
}

duplicate_env='WEB_PORT=3000
WEB_PORT=3001'
assert_rejected 'duplicate publish port' "$duplicate_env"
assert_rejected 'malformed quoted port' 'WEB_PORT="3000 trailing'
assert_rejected 'quoted port extra text' 'WEB_PORT="3000" trailing'
assert_rejected 'unquoted comment without whitespace' 'WEB_PORT=3000#comment'
continuation_env='ENROLLMENT_PORT=3443
printf ENROLLMENT_SIDE_EFFECT'
assert_rejected 'newline continuation' "$continuation_env"
assert_rejected 'single quote escape' "WEB_PORT='34\\'43'"
assert_rejected 'double quote escape' 'WEB_PORT="34\\"43"'
control_env=$(printf 'WEB_PORT="30\\t00"')
assert_rejected 'quoted control character' "$control_env"
assert_rejected 'single quote trailing text' "WEB_PORT='3000' trailing"
assert_rejected 'double quote trailing text' 'WEB_PORT="3000" trailing'

TEST_TMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/proxycore-install-env.XXXXXX")
TEST_OWNER_PID=${BASHPID:-$$}
cleanup_test_tmp() {
  [ "${BASHPID:-$$}" = "$TEST_OWNER_PID" ] || return 0
  rm -rf "$TEST_TMP_DIR"
}
trap cleanup_test_tmp EXIT HUP INT TERM

assert_existing_env_rejected() {
  label=$1
  env_path=$2
  output=''
  if output=$(
    (
      source_installer
      preflight_runtime_values "$env_path"
    ) 2>&1
  ); then
    printf '%s was accepted\n' "$label" >&2
    exit 1
  fi
  case "$output" in
    *ENROLLMENT_SIDE_EFFECT*|*PROXYCORE_MASTER_KEY*)
      printf '%s leaked or executed data\n' "$label" >&2
      exit 1
      ;;
  esac
}

printf 'WEB_PORT=3000\n' > "$TEST_TMP_DIR/target.env"
ln -s target.env "$TEST_TMP_DIR/.env"
assert_existing_env_rejected 'symlink .env' "$TEST_TMP_DIR/.env"
rm -f "$TEST_TMP_DIR/.env"
mkdir "$TEST_TMP_DIR/.env"
assert_existing_env_rejected 'directory .env' "$TEST_TMP_DIR/.env"
rmdir "$TEST_TMP_DIR/.env"

printf 'UNKNOWN_SETTING=preserve\n WEB_PORT = "3000" # existing\n' > "$TEST_TMP_DIR/.env"
(
  cd "$TEST_TMP_DIR"
  source_installer
  ensure_env_key WEB_PORT 3999 >/dev/null
  ensure_env_key ENROLLMENT_PORT 3443 >/dev/null
)
[ "$(grep -c '^UNKNOWN_SETTING=preserve$' "$TEST_TMP_DIR/.env")" -eq 1 ] || exit 1
[ "$(grep -c '^ WEB_PORT = "3000" # existing$' "$TEST_TMP_DIR/.env")" -eq 1 ] || exit 1
[ "$(grep -c '^ENROLLMENT_PORT=3443$' "$TEST_TMP_DIR/.env")" -eq 1 ] || exit 1
[ "$(stat -c '%a' "$TEST_TMP_DIR/.env")" = 600 ] || {
  printf 'atomic append did not retain restrictive mode\n' >&2
  exit 1
}
[ -z "$(find "$TEST_TMP_DIR" -maxdepth 1 -name '.env.tmp.*' -print -quit)" ] || {
  printf 'atomic append left a temp file\n' >&2
  exit 1
}
(
  cd "$TEST_TMP_DIR"
  source_installer
  mv() { return 1; }
  if (atomic_append_env_key .env ENROLLMENT_PORT 3443 >/dev/null 2>&1); then
    printf 'forced atomic replacement failure was accepted\n' >&2
    exit 1
  fi
  [ -z "$(find . -maxdepth 1 -name '.env.tmp.*' -print -quit)" ] || {
    printf 'failed atomic append left a temp file\n' >&2
    exit 1
  }
)

(
  cd "$TEST_TMP_DIR"
  rm -f .env
  source_installer
  need_cmd() { :; }
  random_b64() { printf 'dummy-master-key'; }
  random_password() { printf 'dummy-password'; }
  WEB_PORT=3000 ENROLLMENT_PORT=3443 DNS_PORT=53
  POSTGRES_USER=proxycore POSTGRES_DB=proxycore
  ensure_env >/dev/null
)
[ "$(stat -c '%a' "$TEST_TMP_DIR/.env")" = 600 ] || {
  printf 'atomic creation did not set restrictive mode\n' >&2
  exit 1
}
[ -n "$(grep -F 'PROXYCORE_MASTER_KEY_BASE64=dummy-master-key' "$TEST_TMP_DIR/.env")" ] || exit 1
[ -z "$(find "$TEST_TMP_DIR" -maxdepth 1 -name '.env.tmp.*' -print -quit)" ] || {
  printf 'atomic creation left a temp file\n' >&2
  exit 1
}
