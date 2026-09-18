#!/bin/sh
# Trusted repository helper for atomic installer environment-file updates.
# It is sourced by install.sh after repository synchronization and never
# evaluates or prints .env content.

INSTALL_ENV_HELPER_LOADED=1
ENV_TEMP_FILE=''

cleanup_env_temp() {
  status=$?
  trap - 0 1 2 15
  if [ -n "${ENV_TEMP_FILE:-}" ]; then
    rm -f "$ENV_TEMP_FILE" 2>/dev/null || :
  fi
  ENV_TEMP_FILE=''
  exit "$status"
}

begin_env_temp() {
  env_target="$1"
  ENV_TEMP_FILE=''
  trap cleanup_env_temp 0 1 2 15
  env_old_umask=$(umask)
  umask 077
  if ! ENV_TEMP_FILE=$(mktemp "${env_target}.tmp.XXXXXX"); then
    umask "$env_old_umask"
    die "unable to create private .env temporary file"
  fi
  umask "$env_old_umask"
  chmod 600 "$ENV_TEMP_FILE" || die "unable to secure .env temporary file"
}

finish_env_temp() {
  ENV_TEMP_FILE=''
  trap - 0 1 2 15
}

atomic_append_env_key() {
  env_target="$1"
  env_key="$2"
  env_value="$3"
  assert_env_file_regular "$env_target"
  begin_env_temp "$env_target"
  cat "$env_target" > "$ENV_TEMP_FILE" || die "unable to copy .env safely"
  printf '%s=%s\n' "$env_key" "$env_value" >> "$ENV_TEMP_FILE" || die "unable to update .env safely"
  chmod 600 "$ENV_TEMP_FILE" || die "unable to secure .env temporary file"
  assert_env_file_regular "$env_target"
  mv -f "$ENV_TEMP_FILE" "$env_target" || die "unable to replace .env atomically"
  finish_env_temp
}

create_env_file() {
  env_target="$1"
  assert_env_file_regular "$env_target"
  begin_env_temp "$env_target"
  cat > "$ENV_TEMP_FILE" <<EOF
PROXYCORE_MASTER_KEY_BASE64=${master_key}
POSTGRES_DB=${POSTGRES_DB}
POSTGRES_USER=${POSTGRES_USER}
POSTGRES_PASSWORD=${postgres_password}
WEB_PORT=${WEB_PORT}
ENROLLMENT_PORT=${ENROLLMENT_PORT}
DNS_PORT=${DNS_PORT}
PROXY_INGRESS_IPV4=
PROXY_INGRESS_IPV6=
ACME_DIRECTORY_URL=https://acme-staging-v02.api.letsencrypt.org/directory
ACME_PRODUCTION_DIRECTORY_URL=https://acme-v02.api.letsencrypt.org/directory
PROXYCORE_ACME_EMAIL=
PROXYCORE_CERT_RENEWAL_INTERVAL=1h
NGINX_ACME_UPSTREAM=http://127.0.0.1:${WEB_PORT}
EOF
  chmod 600 "$ENV_TEMP_FILE" || die "unable to secure .env temporary file"
  assert_env_file_regular "$env_target"
  mv -f "$ENV_TEMP_FILE" "$env_target" || die "unable to create .env atomically"
  finish_env_temp
}
