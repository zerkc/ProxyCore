# Runbook — Primary/Node local export and import (Phase 1)

This runbook covers the Phase 1 local-only round-trip: exporting a
snapshot from installation A and importing it into installation B **on
the same machine**, without any networking, enrollment, or
synchronization transport. It is the smallest evidence that one
installation can reproduce another installation's data plane.

## Prerequisites

- Two installations of ProxyCore, A and B, on the same host (or
  reachable through any out-of-band channel). Each installation has its
  own data directory and its own PostgreSQL database.
- Both installations are at version 0.3.0 or later.
- An Owner bootstrap has been completed on A. B may be un-bootstrapped
  (a fresh install) or already-bootstrapped with a different
  configuration.
- The cluster KEK has been generated on A and out-of-band transferred to
  B. Phase 1 does not provide an automated enrollment channel; the
  transfer is the operator's responsibility for the duration of this
  runbook.

## Phase 1 export from A

1. Stop A's data plane so the exported snapshot is not racing with an
   in-flight apply. Phase 1 does not yet freeze writes; this is an
   operator-side pause.

   ```sh
   docker compose stop coredns nginx worker
   ```

2. Use the new exporter entry point (added in `apps/api/internal/snapshot/export.go`)
   to produce a sealed envelope:

   ```sh
   proxycore-admin snapshot export \
     --output /tmp/snapshot.json \
     --cluster-kek /etc/proxycore/cluster.kek
   ```

3. Verify the file looks right: the top-level keys are `transient`,
   `nodeLocal`, `replicated`, and `contentHash`. The replicated section
   carries `configuration`, `secrets`, and `owners`. The secrets list
   shows a `v1.kek:<iv>:<tag>:<ciphertext>` envelope per secret.

4. Confirm the snapshot validates locally before transferring it:

   ```sh
   proxycore-admin snapshot validate --input /tmp/snapshot.json
   ```

   A clean run reports `OK`. Any rejection lists the offending field
   and code (`STRUCTURE`, `COMPATIBILITY`, `SECRET_DECRYPT`,
   `SECRET_ENVELOPE`, `OWNER_HASH`, `OWNER_ROLE`).

## Phase 1 import into B

1. Stop B's data plane (same instructions as A). Phase 1 import does
   not yet accept concurrent writers.

2. Out-of-band transfer `/tmp/snapshot.json` to B and the cluster KEK
   file to B's `/etc/proxycore/cluster.kek`. Confirm ownership and
   `chmod 0600` both files.

3. Run the import. The importer rewrites NodeLocal fields with B's
   locally configured ingress before persisting:

   ```sh
   proxycore-admin snapshot import \
     --input /tmp/snapshot.json \
     --cluster-kek /etc/proxycore/cluster.kek \
     --local-node-id "$(cat /etc/proxycore/node-id)" \
     --archive-ttl 720h
   ```

   The importer:
   - validates the envelope;
   - archives B's existing standalone configuration with a 30-day TTL
     (the default; the `--archive-ttl` flag overrides it);
   - writes the imported desired state as a new revision;
   - enqueues an apply job tagged `source: "import"` so the audit log
     records the provenance.

4. Validate the imported snapshot is now active:

   ```sh
   proxycore-admin snapshot status
   ```

   The output shows the latest applied snapshot's content hash, source
   primary id, leadership generation, and applied timestamp.

5. Verify per-node DNS answers. From B's host, point `dig` at B's
   CoreDNS (port 53) and compare against A's:

   ```sh
   # DNS-only record: same answer on both nodes
   dig @192.0.2.20 dns-only.home.arpa +short   # -> 192.0.1.10

   # Proxied record: answer is B's ingress on B, A's ingress on A
   dig @192.0.2.10 app.home.arpa +short        # -> 192.0.2.10
   dig @192.0.2.20 app.home.arpa +short        # -> 192.0.2.20
   ```

6. Verify the Nginx candidate. From B's host:

   ```sh
   docker compose exec nginx nginx -t -c /var/lib/proxycore/nginx/nginx.conf
   ```

   The candidate must load without errors. Both nodes should route the
   same hostname to the same configured origin (the per-node ingress
   only affects the DNS answer, not the Nginx upstreams).

## Atomic apply and rollback

The existing worker apply pipeline handles validation, promotion, and
reload. Phase 1 tags imported jobs with `source: "import"` so the audit
log records the provenance; the pipeline itself is unchanged. A failed
import keeps the previous applied revision and surfaces the failure in
the same way any other apply failure surfaces.

To verify the rollback path:

1. Force a bad snapshot: tamper with the content hash on a copy of
   `/tmp/snapshot.json` and run the import with that file. The validator
   rejects the envelope; B's active data plane is unchanged.

2. Force a bad secret: wrap a secret value with the wrong KEK and run
   the import. The validator reports `SECRET_DECRYPT`; B's active data
   plane is unchanged.

3. Force a bad Nginx candidate: run an apply that produces an invalid
   Nginx config (out of scope for Phase 1; use the existing failure
   mode). The worker rolls back; the previous revision remains active.

## Retention

The standalone archive retention worker runs once per hour (default)
and removes archives whose `expires_at` is in the past. To inspect:

```sh
proxycore-admin snapshot archive list
proxycore-admin snapshot archive purge --dry-run
```

To force an immediate purge:

```sh
proxycore-admin snapshot archive purge
```

The 30-day default is the resolution recorded in the PRD open-decision
round; override per-archive if an operator explicitly preserves a
specific archive (the archive retention API exposes a `purge=false`
flag for that case).

## Failure paths observed during Phase 1 development

- A snapshot whose `transient.sourcePrimaryId` does not match the
  enrolled primary id is rejected by the validator. Re-export from A
  with the current primary installation id.
- A snapshot whose `transient.leadershipGeneration` is strictly older
  than B's local `latest_known_generation` is rejected as a stale
  replay. The error is logged with the offending generation.
- A snapshot whose `nodeLocal.role` is incompatible with B's current
  state machine is rejected. Phase 1 only accepts role `node` for the
  importer; promotion is a Phase 4 workflow.

## Recovery

If the import corrupts B's state, restore the archived standalone
configuration:

```sh
proxycore-admin snapshot archive restore --id <archive-id>
```

This re-applies the snapshot captured at the moment B became a node.
The archive is removed from the retention queue and the active data
plane returns to B's pre-NODE state. Phase 2 will introduce an automated
restore UI; until then, the operator invokes it manually.

## Phase 1 acceptance checklist

- [ ] Export from A produces a sealed envelope.
- [ ] Import into B archives B's prior state with a 30-day TTL.
- [ ] Import rewrites NodeLocal with B's locally configured ingress.
- [ ] DNS-only answers match between A and B.
- [ ] Proxied DNS answers resolve to A's ingress on A and B's ingress on B.
- [ ] Both Nginx candidates route the same hostname to the same origin.
- [ ] A deliberately corrupt snapshot is rejected without modifying the
      active data plane.
- [ ] A snapshot with an unusable secret is rejected without modifying
      the active data plane.
