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

The import replaces the 16 configuration tables inside one SQLite write
transaction (`BEGIN IMMEDIATE`). Before reinserting, it also deletes the runtime
rows that reference those tables: all `sessions`, apply jobs and applied
snapshots, sync and enrollment state, and `audit_events` rows that name an actor.
Actor-less audit rows, such as failed logins, are kept. A validation, delete, or
reinsert failure before commit leaves the database untouched.

Because every session is deleted, **every user is logged out after a real
import, including the Owner who ran it.** Sign in again before the next request.

After the database transaction commits, ProxyCore restores `env/env` and the
certificate files, then triggers the apply. If the apply succeeds,
`appliedPostImport` is true in the successful `ImportReport`. An apply failure
is reported as an import failure, but it rolls back **nothing**: the restored
database transaction is already committed. Investigate the apply result and
trigger `POST /api/apply` after correcting the operational problem.

## Using the dashboard

The same export and import are available in the dashboard at
**Backup** (`/dashboard/backup`) without `curl`. The page calls the same
Owner-only endpoints described above.

Requirements:

- Sign in as an **Owner**. Operators and anonymous sessions are rejected and the
  page reports that Owner access is required.
- The installation must have a master key configured. Without one the backup
  endpoints answer `503` and the page reports that backup is unavailable.
- The passphrase is optional. If you set one on export, the same passphrase is
  required to import that bundle. The page sends it base64url-encoded in the
  request header or multipart field, never in the URL.
- The Backup entry is only shown on primary surfaces. It is hidden on a NODE
  that has been converted to NODE-only mode.

Export:

1. Open **Backup**, optionally enter a passphrase, and select **Export backup**.
2. The browser downloads the ZIP using the filename supplied by the server
   (`proxycore-backup-<UTC timestamp>.zip`). Store it in the protected backup
   location; it carries configuration, certificates, and the master-key
   environment.

Import:

1. Select the `.zip` bundle and enter its passphrase if it has one.
2. Select **Preview import**. This runs a dry run (`dry_run=true`) and shows the
   bundle metadata (created time, exporter and format version, node ID), the
   table list with row counts and actions, whether the environment would
   change, how many certificates would be restored, and any compatibility
   conflicts. Nothing is written.
3. Review the preview, then select **Restore backup**. This runs the real
   import (`dry_run=false`) and replaces all configuration on the installation.
4. **All sessions are invalidated after a real import, including yours.** The
   page clears its local session state and sends you to the login screen.
   Sign in again with an account from the restored configuration.

Errors the page reports:

| Situation                        | Message meaning                                                                               |
| -------------------------------- | --------------------------------------------------------------------------------------------- |
| `409` `master_key_mismatch`      | The bundle was made with a different master key. Configure the original master key and retry. |
| `401` passphrase mismatch        | The passphrase is incorrect for this bundle.                                                  |
| `400` passphrase required        | The bundle is encrypted; enter its passphrase.                                                |
| `400` invalid archive or version | The file is corrupt or from an unsupported version.                                           |
| `413`                            | The bundle exceeds the 4 GiB limit.                                                           |
| `503`                            | Backup is unavailable because the master key is not configured.                               |
| `401`/`403`                      | Owner access is required.                                                                     |

## Backing up the SQLite file

The bundle is a configuration export, not a full database backup. Runtime state
(sessions, audit, jobs, sync and enrollment rows) is not in it. ProxyCore has no
command that copies the database file: `proxycore-admin` provides
`reset-password` and `snapshot status` only. The internal `sqlitedb.Snapshot`
helper (`VACUUM INTO`) is not wired to a CLI or endpoint.

Use one of these two methods. Both need the `sqlite3` CLI on the host or in a
temporary container; the ProxyCore images do not include it.

- **Quiesced copy.** Stop the writers, copy the file, and start them again:

  ```sh
  docker compose stop worker api
  # Copy proxycore.db from the sqlite-data volume, plus proxycore.db-wal and
  # proxycore.db-shm if they exist, to protected storage.
  docker compose start api worker
  ```

- **Live consistent copy.** `VACUUM INTO` writes a consistent snapshot to a new
  file and never modifies the source:

  ```sh
  sqlite3 /path/to/proxycore.db \
    "VACUUM INTO '/protected/backups/proxycore-$(date -u +%Y%m%dT%H%M%SZ).db'"
  ```

Do not copy only the main `proxycore.db` file while the stack runs. With WAL,
recent commits can sit in the `-wal` file, so a plain copy can miss them or
capture an inconsistent state.

Before moving a copy to the backup store, check it:

```sh
sqlite3 /protected/backups/proxycore-....db "PRAGMA integrity_check"
```

The copy holds encrypted secrets and the master key is not in it. Store
`PROXYCORE_MASTER_KEY_BASE64` separately, as the pre-flight checklist requires.

To restore a raw copy, stop `api` and `worker`, replace `proxycore.db`, remove
any `-wal` and `-shm` files that do not belong to the copy, then start the
services. The API applies any missing migrations on start.

## Restore on a fresh host

Use this recipe when the original host is unavailable:

1. Install ProxyCore from a clean box with `scripts/install.sh`, supplying the
   existing `PROXYCORE_MASTER_KEY_BASE64` from the previous installation.
2. Bootstrap a throwaway Owner on the new installation and put that Owner
   session in `$COOKIES`. A new host has its own SQLite file, so the existing
   Owner session cannot be reused.
3. POST `/api/backup/import?dry_run=true` with the bundle and passphrase. Check
   the report for the expected installation identity, table counts, environment
   change, and certificate list.
4. POST `/api/backup/import?dry_run=false` with the same bundle and passphrase.
   Confirm the successful report and `appliedPostImport` result. The import
   clears all sessions, so sign in again and replace `$COOKIES` before step 5.
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

- SQLite runtime rows for the tables above (use the file backup described below
  for those)
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
