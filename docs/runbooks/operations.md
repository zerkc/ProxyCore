# ProxyCore Operations Runbook

## Nginx configuration persistence

On every successful apply, the promoted Nginx config is persisted to the stable
file `<candidates>/nginx-live.conf` on the shared candidates volume **after** it
is confirmed inside the container. A copy of the previous stable config is saved
as `nginx-previous-live.conf`. When the Nginx container restarts or is recreated,
the entrypoint script restores the stable config (validated with `nginx -t`) before
Nginx starts. Legacy candidates in `<revision>/nginx/nginx.conf` are used as a
best-effort fallback for installations created before this fix. If no valid config
exists, Nginx starts with the baked default and logs a warning; an apply restores
full functionality.

The stable file is updated atomically only after the container promotion succeeds.
If persistence fails, the container is restored to its pre-promotion state and the
apply fails. Rollback restores both the container config and the persistent stable
file to the last known-good state.

## Apply and rollback

The worker renders a revision, stages it, validates it, promotes it, reloads
the target service, and checks health. A failed health check must result in a
rollback or an explicit recovery-required job.

The worker listens for PostgreSQL job notifications and wakes immediately when
an apply is committed. It also reconciles the durable queue every 5 minutes by
default (`WORKER_RECONCILIATION_INTERVAL_MS`). Jobs are claimed with a lease
and are re-queued after the lease expires, so a missed notification or a
restart does not discard desired state or an outstanding apply.

Saving a zone or DNS record creates its revision and queues an immediate
combined CoreDNS/Nginx apply. Consecutive saves are intentionally not batched;
inspect `/api/status` when several edits are made in succession.

Inspect current jobs and status through the authenticated API:

```sh
curl -b cookies.txt http://localhost:3000/api/status
```

Do not edit CoreDNS or Nginx files while an apply is running. Preserve the
candidate checksum and correlation id when reporting an incident.

## Service-control boundary

Only `control` receives the Docker Engine socket. `web` and `worker` do not.
The worker sends a fixed JSON-lines operation over `/run/proxycore/control.sock`;
there is no arbitrary command, shell, or directive field. If the helper socket
is unavailable, stop applying and investigate the helper rather than granting
the worker a socket.

## Master-key rotation

1. Stop mutations and take a verified backup of the PostgreSQL volume.
2. Keep the old key available only in a protected operator environment.
3. Read each encrypted secret through the old key and write a new ciphertext
   with the new 32-byte key in one controlled migration.
4. Verify certificate private keys and provider credentials can be decrypted.
5. Update `PROXYCORE_MASTER_KEY_BASE64`, restart control-plane services, and
   remove the old key from process environments.

Never log either key or decrypted secret values.

## Cloudflare credential rotation

Create a new token scoped to the required zone and `_acme-challenge` TXT
operations, update the encrypted provider secret, issue a staging challenge,
then revoke the old token. Ordinary DNS records must not be modified by the
DNS-01 adapter.

## Certificate incidents

- A renewal failure keeps the last active certificate.
- If expiry is approaching, use staging first and inspect challenge reachability.
- HTTP-01 requires public port 80 to reach the challenge handler.
- DNS-01 requires propagation of only `_acme-challenge` TXT records.
- Never delete the active certificate during cleanup.
- On apply, the worker materializes certificate PEM/key and Basic Auth htpasswd
  files into the Nginx candidate directory under the shared `candidates` volume.
  Nginx config references those absolute candidate paths; inspect them under
  `/var/lib/proxycore/candidates/<revisionId>/nginx/` when debugging `nginx -t`.

## Retention

Retention removes old or oversized operational artifacts only. It must keep
desired state, the active applied revision, and active certificate keys.

## PostgreSQL to SQLite conversion

Existing PostgreSQL installations move to SQLite once, offline, with
`apps/api/cmd/convert`. It is the only code that still reads PostgreSQL; the
source is read inside one `REPEATABLE READ READ ONLY` transaction and is never
written.

1. Stop the api and the worker first so the snapshot is consistent and no new
   writes are lost. Keep PostgreSQL running and untouched.
2. Run the converter with the same master key the installation already uses:

   ```sh
   export PROXYCORE_CONVERT_PG_URL='postgres://...'      # never logged
   export PROXYCORE_MASTER_KEY_BASE64='...'              # same key as today
   go run ./apps/api/cmd/convert --out /var/lib/proxycore/proxycore.db
   ```

   `--out` must not exist (there is no `--force`). The converter writes
   `<out>.tmp`, then validates it independently of the copy: per-table row
   counts and canonical hashes on both sides, `foreign_key_check`,
   `integrity_check`, schema version, every encrypted secret decrypts with the
   master key and is byte-identical to PostgreSQL, `config_revisions.checksum`
   and `applied_snapshots.content_hash` are recomputed, and snapshot BLOBs are
   compared. Only then is the file published atomically and
   `<out>.converted.json` written. That marker (tool version, UTC time,
   per-table counts and hashes, warnings, source schema fingerprint, no
   secrets) is the proof that the file was converted and validated; a database
   without it must not be used.
3. Read the warnings. Dangling references in `enrollment_tokens`,
   `enrollment_grants` and `enrolled_nodes` (their foreign keys were dropped
   in PostgreSQL) and older-schema gaps are copied or defaulted and listed, not
   fatal.
4. Start the new release with `PROXYCORE_SQLITE_PATH` pointing at the file, on
   a volume shared by the api and the worker.

Exit codes: `0` converted and validated, `1` validation or fidelity failure
(nothing is published, temporary files are removed), `2` usage error or output
already present, `3` PostgreSQL unreachable or a schema or data shape the
converter does not know.

Rollback: redeploy the previous release against the untouched PostgreSQL
database. Changes made after cutover on SQLite are not copied back.
