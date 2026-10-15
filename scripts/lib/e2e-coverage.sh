#!/usr/bin/env bash
# Endpoint coverage check for the Docker endpoints e2e.
#
# Reads the routes registered in apps/api/internal/httpserver/server.go routes()
# (resolving the Go path constants) plus the :3443 enrollment listener endpoints,
# and fails when one of them has no matching "METHOD path" line in the hits file
# written by scripts/lib/e2e-docker.sh.
#
# Usage: E2E_HITS_FILE=<file> bash scripts/lib/e2e-coverage.sh [--list]
#   --list prints the route inventory only (no hits file needed).
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
exec python3 -I - "$ROOT" "${E2E_HITS_FILE:-}" "${1:-}" <<'PY'
import glob, re, sys

root, hits_file, mode = sys.argv[1], sys.argv[2], sys.argv[3]
pkg = root + "/apps/api/internal/httpserver"
server = open(pkg + "/server.go").read()

consts = {}
for f in glob.glob(pkg + "/*.go"):
    if f.endswith("_test.go"):
        continue
    src = open(f).read()
    for m in re.finditer(r'^\s*(?:const\s+)?(\w+)\s*=\s*((?:"[^"]*"|\w+)(?:\s*\+\s*(?:"[^"]*"|\w+))*)\s*$', src, re.M):
        consts[m.group(1)] = m.group(2)

def resolve(expr, depth=0):
    out = ""
    for part in re.split(r'\s*\+\s*', expr.strip()):
        if part.startswith('"'):
            out += part.strip('"')
        elif part in consts and depth < 8:
            out += resolve(consts[part], depth + 1)
        else:
            raise SystemExit("cannot resolve Go constant %r in server.go routes()" % part)
    return out

body = server[server.index("func (s *Server) routes()"):]
body = body[: body.index("\n}\n")]
routes = []  # (method or "*", pattern)
for m in re.finditer(r's\.mux\.Handle(?:Func)?\(\s*((?:"[^"]*"|\w+)(?:\s*\+\s*(?:"[^"]*"|\w+))*)\s*,', body):
    raw = m.group(1)
    if raw.strip() == "path":
        continue  # method-less workflow paths, expanded below
    full = resolve(raw)
    if " " in full:
        method, pattern = full.split(" ", 1)
    else:
        method, pattern = "*", full
    routes.append((method, pattern))
loop = re.search(r'range \[\]string\{([^}]*)\}', body)
if loop:
    for name in re.split(r'\s*,\s*', loop.group(1).strip()):
        if name:
            routes.append(("*", resolve(name)))

# The :3443 enrollment TLS listener is wired in cmd/server/main.go, not routes().
for name in ["EnrollmentIdentityProofPath", "EnrollmentGrantExchangePath", "EnrollmentSnapshotByTokenPath",
             "EnrollmentSnapshotAcknowledgementPath", "EnrollmentSnapshotPublicationPath"]:
    routes.append(("*", resolve(name)))

if mode == "--list":
    for method, pattern in routes:
        print("%-6s %s" % (method, pattern))
    sys.exit(0)

hits = set()
for line in open(hits_file):
    parts = line.split()
    if len(parts) == 2:
        hits.add((parts[0], parts[1]))

missing = []
for method, pattern in routes:
    rx = re.compile("^" + re.sub(r"\\\{[^}]+\\\}", "[^/]+", re.escape(pattern)) + "$")
    ok = any(rx.match(p) and (method == "*" or method == mth) for mth, p in hits)
    if not ok:
        missing.append("%s %s" % (method, pattern))
print("coverage: %d routes, %d hit, %d missing" % (len(routes), len(routes) - len(missing), len(missing)))
for item in missing:
    print("  NOT HIT: " + item)
sys.exit(1 if missing else 0)
PY
