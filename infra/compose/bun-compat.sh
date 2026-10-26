#!/bin/sh
# Backward-compatibility shim. This is NOT Bun.
#
# Installs created before 0.6.22 have a host compose.yaml with
# `command: ["bun","run","worker"]` and `["bun","run","control"]`. Dashboard
# updates only pull images and recreate containers; they never rewrite that
# file, so after an update the old command lines run against this image, which
# no longer ships Bun. This script maps exactly those two commands to the Go
# binaries and rejects everything else.
#
# `exec` replaces the shell, so the Go process becomes PID 1 and receives
# SIGTERM from `docker stop` directly. The working directory is ignored.
#
# It will be removed in a later release. To drop it from an old host, refresh
# compose.yaml (re-run the installer); new installs call the binaries directly.

if [ "$#" -eq 2 ] && [ "$1" = run ]; then
  case "$2" in
    worker) exec /usr/local/bin/proxycore-worker ;;
    control) exec /usr/local/bin/proxycore-control ;;
  esac
fi

echo "bun is not available in this image; this shim only supports \`bun run worker\` and \`bun run control\` for old compose files" >&2
exit 1
