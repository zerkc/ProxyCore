# Backup and restore runbook

## Overview

ProxyCore can export the configuration required to rebuild an installation into
one ZIP bundle and restore that bundle through the Owner-only API. The bundle
contains the configuration tables, the deployed `.env`, and certificates from
the latest applied candidate. It is intended for disaster recovery on the same
host or on a clean host with the same installation identity and master key.

The API streams the ZIP; it does not keep a server-side backup file. In the
export example below, `curl -o` writes the bundle into the operator's current
directory as `proxycore-backup-<UTC timestamp>.zip`. Store that file in the
protected backup location used by the Owner. Treat it as sensitive: it carries
configuration, certificate material, and the deployed master-key environment.

## Pre-flight checklist

Before exporting or importing:

- [ ] The session in `$COOKIES` belongs to an active **Owner**. Operators and
      anonymous requests are rejected.
- [ ] The installation has at least one applied revision. Confirm the current
      applied revision in the dashboard or through `GET /api/status` before
      exporting.
- [ ] `PROXYCORE_MASTER_KEY_BASE64` from the installation is recorded in a
      protected location. A fresh host must use the same key before it can read
      the restored encrypted secrets.
- [ ] The backup passphrase is available if the bundle was exported with one.
- [ ] The bundle and the cookie jar will not be committed to source control or
      copied into an unprotected temporary location.

Keep the cookie jar authenticated as the Owner for both dry-run and real
imports. Do not use a session cookie from the old host after its sessions have
been intentionally discarded.

## Exporting a backup

Set `COOKIES` to the authenticated Owner cookie jar and set
`BACKUP_PASSPHRASE` to the passphrase to wrap the ZIP. The export is a streamed
`POST`:

```bash
curl -sS -b "$COOKIES" \
  -o proxycore-backup-$(date -u +%Y-%m-%dT%H-%M-%SZ).zip \
  -H "X-Proxycore-Backup-Passphrase: $(echo -n "$BACKUP_PASSPHRASE" | base64 -w0)" \
  -X POST http://127.0.0.1:3000/api/backup/export
```

A successful response has these transport headers:

- `Content-Type: application/zip`
- `Content-Disposition: attachment; filename="..."`
- HTTP trailer `Audit-Version: <manifest sha256>` after the ZIP body has been
  streamed. The value is the manifest digest for the exported bundle.

`X-Proxycore-Backup-Passphrase` is the base64url encoding of the passphrase's
UTF-8 bytes, not the raw passphrase. The command above is convenient for normal
ASCII values. For material whose encoded output contains `+`, `/`, or `=`, use
an equivalent that converts to the URL-safe alphabet and removes padding. For
non-ASCII material, preserve the UTF-8 bytes with `--data-urlencode` or another
byte-preserving encoder; the final header value must still be base64url.

The response body is the ZIP. Verify that the file exists and protect it before
moving it to the backup store. A passphrase-protected bundle must be imported
with the same passphrase.

## Dry-run import

Always preview an import before allowing it to replace configuration. The
passphrase is sent as the multipart `passphrase` field, using the same
base64url encoding as the export header:

```bash
curl -sS -b "$COOKIES" \
  -X POST "http://127.0.0.1:3000/api/backup/import?dry_run=true" \
  -F "bundle=@proxycore-backup-...zip" \
  -F "passphrase=$(printf %s "$BACKUP_PASSPHRASE" | base64 -w0)"
```

The response is an `ImportReport`. A representative response shape is:

```json
{
  "dryRun": true,
  "bundleSha256": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
  "formatVersion": "1",
  "exporterVersion": "proxycore",
  "createdAt": "2026-01-02T03:04:05Z",
  "installationId": "550e8400-e29b-41d4-a716-446655440000",
  "nodeId": "660e8400-e29b-41d4-a716-446655440001",
  "tables": [
    {"name": "users", "rowCount": 1, "action": "truncate+reinsert"}
  ],
  "envWouldChange": true,
  "certsToRestore": ["certs/example.crt", "certs/example.key"],
  "appliedPostImport": false
}
```

The real report contains one table preview for each configuration table.
`dryRun: true` means no configuration rows, environment file, certificate,
or apply job is written. The report is a sanity check for the installation
identity, table counts, environment change, and certificate paths. A bundle
with a wrong passphrase, wrong master key, invalid checksum, unsupported
version, or unsafe archive path must not be used.

## Real import

After reviewing the dry-run report, use the same bundle, cookie jar, and
passphrase with `dry_run=false`:

```bash
curl -sS -b "$COOKIES" \
  -X POST "http://127.0.0.1:3000/api/backup/import?dry_run=false" \
  -F "bundle=@proxycore-backup-...zip" \
  -F "passphrase=$(printf %s "$BACKUP_PASSPHRASE" | base64 -w0)"
```

The import truncates and reinserts the 16 configuration tables inside one
PostgreSQL transaction. A validation, truncate, or reinsert failure before
commit leaves the database untouched. Existing rows in those configuration
tables are intentionally replaced; runtime tables remain in place.

After the database transaction commits, ProxyCore restores `env/env` and the
certificate files, then triggers the apply. If the apply succeeds,
`appliedPostImport` is true in the successful `ImportReport`. An apply failure
is reported as an import failure, but it rolls back **nothing**: the restored
database transaction is already committed. Investigate the apply result and
trigger `POST /api/apply` after correcting the operational problem.

## Restore on a fresh host

Use this recipe when the original host is unavailable:

1. Install ProxyCore from a clean box with `scripts/install.sh`, supplying the
   existing `PROXYCORE_MASTER_KEY_BASE64` from the previous installation.
2. Bootstrap a throwaway Owner on the new installation, or use the existing
   Owner if the host shares the same database. Put that Owner session in
   `$COOKIES`.
3. POST `/api/backup/import?dry_run=true` with the bundle and passphrase. Check
   the report for the expected installation identity, table counts, environment
   change, and certificate list.
4. POST `/api/backup/import?dry_run=false` with the same bundle and passphrase.
   Confirm the successful report and `appliedPostImport` result.
5. Verify the restored data through `GET /api/zones`,
   `GET /api/certificates`, and `GET /api/users`; compare the results with the
   pre-restore installation.
6. If the engine did not fire the apply automatically, trigger
   `POST /api/apply`, then verify the resulting status and rendered services.

The fresh host must use the same master key and must satisfy the bundle's
installation-identity checks. A different cluster is not a supported target
for this restore path.

## Passphrase guidance

- Use a memorable, randomly chosen passphrase of at least 12 characters. A
  longer passphrase gives the PBKDF2 key-derivation step more useful input.
- Store the passphrase in the same protected recovery location as
  `PROXYCORE_MASTER_KEY_BASE64`, but keep the two values separately labeled.
- Losing the passphrase makes a passphrase-encrypted bundle unreadable. The
  master key alone is not sufficient to open the encrypted envelope.
- Do not put the passphrase in shell history, logs, tickets, source control, or
  an audit value. Clear shell variables and remove temporary copies after the
  restore when the operating procedure permits.

## What is NOT in the bundle

The bundle is a configuration recovery artifact, not a copy of all runtime
state. It excludes these tables:

- `sessions`
- `audit_events`
- `apply_jobs`
- `applied_snapshots`
- `standalone_archives`
- `enrollment_*`
- `node_credentials`
- `enrolled_nodes`
- `enrollment_grants`
- `node_snapshot_acks`
- `sync_attempts`
- `health_observations`
- `operational_artifacts`

It also excludes these volumes or filesystem state:

- PostgreSQL row state for the runtime tables above
- The candidate staging tree outside the latest applied revision
- The promoted CoreDNS zones
- Updater bootstrap state

The Owner session cookie and live database connection strings are not bundled.
CoreDNS and Nginx configurations are rendered deterministically from the
restored desired state on the next apply, so promoted runtime output is
regenerated rather than copied as authoritative state.

## Audit event

Import audit records are written to `audit_events` when the import engine reaches
its audit boundary. A successful real import records action `backup.import`; a
dry-run records `backup.import.dryrun`. The event contains the authenticated
Owner's user ID as the actor, resource type `backup`, and a result of `success`
or `failure`. Post-commit restore or apply failures are recorded as failures
while leaving the committed database restore in place.

The JSON `after_value` is deliberately limited to:

```json
{
  "bundleSha256Prefix": "0123456789abcdef",
  "dryRun": false,
  "success": true
}
```

Only the first 16 hexadecimal characters of the bundle/manifest SHA-256 are
stored. The audit event never stores the passphrase, master key, decrypted
secret or ciphertext plaintext, or certificate bytes. Requests rejected before
a verified bundle reaches the import engine are returned as API errors and do
not expose those values in audit data.
