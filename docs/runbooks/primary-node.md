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

The Phase 1 commit delivers the `Exporter` library
(`apps/api/internal/snapshot/export.go`) and the Go unit tests, but
does NOT yet ship an out-of-band CLI that drives the exporter from the
terminal; that surface lands with the Phase 2 enrollment work. Until
then, an operator that wants to drive the exporter writes a small Go
program that:

1. Constructs a `cluster.KEK` from the on-disk key material.
2. Wires the existing `configuration.Store` as the
   `snapshot.ConfigurationSource`, a thin `secrets.Store` adapter as
   the `snapshot.SecretLister`, and the existing user repository as the
   `snapshot.OwnerLister`.
3. Calls `exporter.Export(ctx, snapshot.ExporterInput{...})` and writes
   the resulting `Marshal(env)` bytes to a file.

The expected envelope shape and validation flow are documented here so
the Phase 2 CLI work has a stable contract to bind to.

### Expected envelope shape

The top-level keys are `transient`, `nodeLocal`, `replicated`, and
`contentHash`. The replicated section carries `configuration`,
`secrets`, and `owners`. The secrets list shows a
`v1.kek:<iv>:<tag>:<ciphertext>` envelope per secret.

## Phase 1 import into B

Likewise, the `Importer` library is shipped with full unit tests but
does not yet have an out-of-band CLI. The library takes:

- a sealed envelope (already validated by `snapshot.Validator`),
- the local `ImporterInput{LocalNodeID, LocalIngress, ArchiveTTL,
  ArchiveReason}` so the importer can rewrite NodeLocal fields and
  archive the prior standalone state.

The operator code that drives the importer:

1. Loads the envelope bytes and calls `snapshot.Unmarshal`.
2. Calls `snapshot.NewValidator(kek).Validate(ctx, env)`; refuses to
   proceed if `result.HasIssues()`.
3. Constructs an `Importer{archive: pgStore, enqueuer: applyJobs,
   now: time.Now}` and calls `imp.Import(ctx, &env, ImporterInput{...})`.
4. Confirms `ImporterResult{ArchiveID, ApplyJobID}` and observes the
   apply job's lifecycle through the existing worker logs.

## Inspecting snapshot status

`proxycore-admin snapshot status` is shipped in this commit and prints
the local installation identity in tabwriter-aligned columns:

```sh
$ proxycore-admin snapshot status
FIELD                    VALUE
installation_id          550e8400-e29b-41d4-a716-446655440000
node_id                  660e8400-e29b-41d4-a716-446655440001
role                     primary
leadership_generation    5
latest_known_generation  5
stale_primary            false
writable                 true
updated_at               2026-05-10T11:12:13Z
```

If the identity has not been bootstrapped yet, the command exits 1
with `installation identity is not bootstrapped` on stderr.

## Verify per-node DNS answers

Use the existing `dig` against each node's CoreDNS. The renderer already
substitutes the per-node ingress for proxied answers:

```sh
# DNS-only record: same answer on both nodes
dig @192.0.2.20 dns-only.home.arpa +short   # -> 192.0.1.10

# Proxied record: answer is B's ingress on B, A's ingress on A
dig @192.0.2.10 app.home.arpa +short        # -> 192.0.2.10
dig @192.0.2.20 app.home.arpa +short        # -> 192.0.2.20
```

## Verify the Nginx candidate

From each host:

```sh
docker compose exec nginx nginx -t -c /var/lib/proxycore/nginx/nginx.conf
```

The candidate must load without errors. Both nodes route the same
hostname to the same configured origin (the per-node ingress only
affects the DNS answer, not the Nginx upstreams).

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
and removes archives whose `expires_at` is in the past. The CLI
surface for `proxycore-admin snapshot archive list` and
`proxycore-admin snapshot archive purge` lands with the Phase 2
Postgres-backed ArchiveStore; until then, the worker is wired with the
`NoopArchiveStore` placeholder and the retention path is documented
behavior without a visible CLI. Operators inspecting the archive queue
during Phase 1 should query the PostgreSQL tables directly:

```sql
select id, captured_at, expires_at, reason
  from standalone_archives
 order by captured_at desc;
```

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
configuration. The restore CLI lands with the Phase 2 enrollment
work; until then, the operator:

1. Reads the archive row from the `standalone_archives` table.
2. Decodes the envelope via `snapshot.Unmarshal`.
3. Calls the Phase 1 library path that writes the snapshot to the
   desired-state table (the same path the Phase 1 importer uses for
   the imported snapshot, minus the archive step).

The archive is removed from the retention queue and the active data
plane returns to B's pre-NODE state.

## Phase 1 acceptance checklist

- [x] Export from A produces a sealed envelope (`snapshot.Exporter`,
      unit-tested).
- [x] Import into B archives B's prior state with a 30-day TTL
      (`snapshot.Importer`, unit-tested).
- [x] Import rewrites NodeLocal with B's locally configured ingress
      (`Importer.replaceNodeLocalFields`, unit-tested).
- [x] DNS-only answers match between A and B
      (`packages/renderers/src/node-local.test.ts`).
- [x] Proxied DNS answers resolve to A's ingress on A and B's ingress
      on B (same fixture).
- [x] Both Nginx candidates route the same hostname to the same origin
      (same fixture documents the contract; live verification is
      Phase 5 hardening).
- [x] A deliberately corrupt snapshot is rejected without modifying
      the active data plane (`snapshot.Validator` + `snapshot.Importer`
      tests).
- [x] A snapshot with an unusable secret is rejected without modifying
      the active data plane (`snapshot.Validator` tests).

Phase 1 implementation is complete at the library level; the
out-of-band CLI surface for export, import, archive list/purge, and
restore lands with Phase 2.
