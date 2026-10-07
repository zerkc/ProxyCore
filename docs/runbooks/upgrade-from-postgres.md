# Upgrade from PostgreSQL to SQLite

Releases that use SQLite no longer ship PostgreSQL. An installation created by
an older release keeps its data in the Docker volume `<project>_postgres-data`
(default project: `proxycore`, so `proxycore_postgres-data`). This runbook moves
that data once, offline, into the new SQLite database. Nothing converts
automatically: the installer and the dashboard updater both stop and point here
when they find the legacy volume and no SQLite database.

## Why

The control plane (Go api) and the worker now share one SQLite file on the
`sqlite-data` volume. The converter (`proxycore-convert`, shipped in the api
image) is the only code that still reads PostgreSQL. It reads inside one
`REPEATABLE READ READ ONLY` transaction, never writes to the source, and
publishes the SQLite file only after an independent validation passes.

## Prerequisites

- Downtime: api and worker must be stopped for the whole conversion. Plan for
  minutes on a small installation; the dashboard and applies are unavailable.
- The master key: `PROXYCORE_MASTER_KEY_BASE64` in the install `.env`, unchanged.
  Secrets stay encrypted with the same key; the converter fails if any secret
  does not decrypt.
- A backup of the legacy volume (below). PostgreSQL cannot start on a read-only
  data directory, so the temporary server mounts the volume read-write even
  though the converter only reads.
- Free disk space for a second copy of the data (the SQLite file).
- The new release checked out in the install directory (the installer does this
  before it stops; or `git pull`).

## Steps

Run everything from the install directory (default `/opt/proxycore`).

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

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Converted, validated, published, marker written. |
| 1 | Validation or fidelity failure. Nothing was published; temporary files were removed. |
| 2 | Usage error, or the output database already exists (the converter has no `--force`). |
| 3 | PostgreSQL unreachable, or a schema or data shape the converter does not know. |

The script exits with the converter's code, or 1 for its own refusals (running
containers, missing master key, missing marker).

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
release checked out, then `docker volume rm proxycore_sqlite-data`, then run
`scripts/convert-postgres.sh` again.

Do not delete `<project>_postgres-data` until the new release has run
correctly for as long as you need the ability to go back. Remove it only when
you are sure, with `docker volume rm proxycore_postgres-data`; the installer and
the updater stop warning once the SQLite database exists, regardless of the
legacy volume.
