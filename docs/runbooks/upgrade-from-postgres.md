# Upgrade from PostgreSQL to SQLite

Releases that use SQLite no longer ship PostgreSQL. An installation created by
an older release keeps its data in the Docker volume `<project>_postgres-data`
(default project: `proxycore`, so `proxycore_postgres-data`). This runbook moves
that data once into the new SQLite database.

Two paths do the same conversion:

- **Automatic (default):** the dashboard's native update. The `migrate` service
  of the new release converts the data during the update. No terminal is needed.
- **Manual (fallback):** `scripts/convert-postgres.sh`, run by hand before the
  update or when the automatic path fails and you want to convert yourself.

## Why

The control plane (Go api) and the worker share one SQLite file on the
`sqlite-data` volume. The converter (`proxycore-convert`, shipped in the api
image and in the `migrate` image) is the only code that still reads PostgreSQL.
It reads inside one `REPEATABLE READ READ ONLY` transaction, never writes to the
source, and publishes the SQLite file only after an independent validation
passes.

## Automatic path: dashboard update

The previous release's updater builds the new images, then runs
`docker compose run --rm migrate`, then restarts `api`, `worker`, `control`
and `nginx`. The `migrate` service (`scripts/migrate-legacy-postgres.sh`) does
the conversion inside that step. It does, in order:

1. Takes a lock (`<data>/.migrate-legacy.lock`) so two runs cannot overlap.
2. Exits 0 without doing anything when there is no legacy volume, or when
   `proxycore.db` and its `.converted.json` marker already exist.
3. Refuses (exit 1) when `proxycore.db` exists without its marker, when partial
   conversion output exists, or when the marker exists without the database.
4. Refuses when `PROXYCORE_MASTER_KEY_BASE64` is empty in `.env`.
5. Refuses when the legacy `postgres` service does not accept connections at
   `postgres:5432`, or rejects `POSTGRES_USER`/`POSTGRES_DB`/`POSTGRES_PASSWORD`.

   Steps 3 to 5 run before anything is stopped. A refusal leaves api and
   worker running, and the update stops before its restart step.
6. Stops the running `api` and `worker` containers. They are the only writers
   of the SQLite database. Stopping them makes the snapshot consistent: a write
   made after the snapshot would otherwise be lost. The legacy `postgres` keeps
   running, because the converter only reads it.
7. Runs `proxycore-convert` as uid 10001, writes `proxycore.db` and its
   marker, and sets both to `10001:10001`, mode `0600`.
8. Leaves `api` and `worker` stopped. The updater's restart step recreates them
   on the new release, and the verify step waits for the api health check.

### What you need before updating

- `PROXYCORE_MASTER_KEY_BASE64` in `.env`, the same key the installation has
  always used. Secrets are copied as ciphertext, not re-encrypted; the converter
  fails if a secret does not decrypt with this key.
- The legacy `postgres` container running. It was running for the whole
  installation, so this is normally already true.
- A backup of the legacy volume (step 2 of the manual path). Keep it until the
  new release has run correctly for as long as you may need to go back.
- Free disk space for a second copy of the data (the SQLite file).

### Downtime

The dashboard and applies are unavailable from step 6 until the updater's
restart completes. Downtime is the conversion time. The automated test with a
small database (a zone, three records, one certificate, one secret) took about
six seconds after the update was accepted, with cached images. Plan for minutes
on a large installation.

### If the update fails

- **Refused before stopping (steps 2 to 5):** nothing was stopped. The old
  release keeps running. Fix the cause shown in the update's `migrate` step
  output, then update again.
- **Conversion failed after stopping (exit 1, 2 or 3, or an interrupt):** the
  `migrate` service removes the partial output and starts `api` and `worker`
  again, because the old updater does not restart them. The updater reports the
  update as failed and restores the source checkout to the previous tag. The
  legacy PostgreSQL data was not changed. Fix the cause (for example a wrong
  master key) and update again. The exit codes are listed below.
- **Dashboard reports a timeout or failure while `migrate` is still running:**
  the updater's apply has a single 10-minute budget for build, migrate, restart
  and verify (the previous release's `apps/updater/cmd/main.go`). If the budget
  ends during a long conversion, the updater's `docker compose run` is cancelled
  but the migrate container keeps running. Do not start another update. Wait for
  it to finish (`docker ps --filter name=migrate` lists it while it runs). This
  path was not tested with a long conversion.
- **Lock left behind:** a killed run leaves `<data>/.migrate-legacy.lock`. The
  next run refuses and says so. When `docker ps -a --filter name=migrate` shows
  no running migrate container, remove the directory from the `sqlite-data`
  volume and retry.

A successful update is idempotent: running the `migrate` service again prints
`Already converted` and exits 0.

## Manual path (fallback)

Run everything from the install directory (default `/opt/proxycore`). Use this
path before the update, or when the automatic path fails and you want to see
every step yourself.

1. Stop the services that write to the database, and any container still using
   the legacy volume (the old release's `postgres`):

   ```sh
   docker compose stop api worker
   docker stop $(docker ps -q --filter volume=proxycore_postgres-data)
   ```

2. Back up the legacy volume (keep this until you have confirmed the upgrade):

   ```sh
   docker run --rm -v proxycore_postgres-data:/data:ro -v "$PWD":/backup alpine \
     tar czf /backup/postgres-data-backup.tgz -C /data .
   ```

3. Convert:

   ```sh
   sh scripts/convert-postgres.sh
   ```

   The script refuses to run while api, worker or any other container uses the
   legacy volume. It starts a temporary PostgreSQL on the legacy volume attached
   only to a private `--internal` Docker network (nothing is published on the
   host), runs `proxycore-convert` from the api image, and writes
   `/var/lib/proxycore-db/proxycore.db` on the `sqlite-data` volume. It reads
   the PostgreSQL credentials from `.env` (`POSTGRES_USER`, `POSTGRES_DB`,
   `POSTGRES_PASSWORD`, falling back to the old defaults) and never prints them
   or the connection URL. The temporary container and network are always
   removed, and the legacy volume is never removed.

4. Validate. A successful run prints the validation result and the file
   `/var/lib/proxycore-db/proxycore.db.converted.json` exists. That marker is the
   proof that the database was converted and validated (tool version, UTC time,
   per-table counts and hashes, warnings, source schema fingerprint, no
   secrets). Never run the new release against a converted-looking file that has
   no marker. To inspect it:

   ```sh
   docker compose run --rm --no-deps -T --entrypoint cat api \
     /var/lib/proxycore-db/proxycore.db.converted.json
   ```

5. Read the warnings (see below), then start the new release:

   ```sh
   sh scripts/install.sh
   ```

   The installer finds the SQLite database and starts the stack; the api
   applies any newer SQLite schema migration on start.

Re-running `scripts/convert-postgres.sh` after success is a no-op that exits 0.
If the database exists without the marker, the script refuses and leaves the
file alone.

## Warnings

Warnings are copied data you should look at, not failures:

- Dangling references in `enrollment_tokens`, `enrollment_grants` and
  `enrolled_nodes`. PostgreSQL had no foreign keys for these three
  relationships, so orphaned rows can exist; they are copied as-is and listed.
- Gaps from older PostgreSQL schemas (columns added by later migrations) are
  defaulted and listed.

Sessions, health history and in-flight jobs are copied as they were. Anything
the converter does not recognise (an unknown table, column or enum value) is
an error, not a warning.

The converter's report (tables, rows, warnings) is printed by both paths. The
automatic path prints it in the updater's `migrate` step output.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Converted, validated, published, marker written. |
| 1 | Validation or fidelity failure. Nothing was published; temporary files were removed. |
| 2 | Usage error, or the output database already exists (the converter has no `--force`). |
| 3 | PostgreSQL unreachable, or a schema or data shape the converter does not know. |

The manual script exits with the converter's code, or 1 for its own refusals
(running containers, missing master key, missing marker). The `migrate` service
uses the same converter codes and adds its own refusals as exit 1.

## Orphaned legacy PostgreSQL container

The new release has no `postgres` service. After the automatic update, the old
`postgres` container keeps running as an orphan (`docker compose` reports
`Found orphan containers ([<project>-postgres-1])`). This is intended: it keeps
the rollback path open, and the converter reads its data. It uses resources
until you remove it.

Remove it only when you will not roll back:

```sh
docker rm -f <project>-postgres-1
```

## Rollback

Until you remove `<project>_postgres-data`, rollback is a redeploy of the
previous release against the untouched PostgreSQL volume:

```sh
docker compose stop api worker
git checkout <previous-tag>
docker compose up -d --build
```

Use `docker compose up` directly: `scripts/install.sh` always moves the checkout
to the tip of its branch. The previous release's compose file starts its own
`postgres` on the legacy volume and reads the `POSTGRES_*` values that are still
in `.env` (the new installer never edits an existing `.env`).

Changes made on SQLite after cutover are not copied back. To retry the
conversion after a rollback, first remove the unwanted SQLite copy (confirm it
holds nothing you need): `docker compose rm -sf api worker` with the new
release checked out, then `docker volume rm proxycore_sqlite-data`, then update
again, or run `scripts/convert-postgres.sh`.

Do not delete `<project>_postgres-data` until the new release has run
correctly for as long as you need the ability to go back. Remove it only when
you are sure, with `docker volume rm proxycore_postgres-data`; the installer and
the updater stop warning once the SQLite database exists, regardless of the
legacy volume.
