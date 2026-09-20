# Full Configuration Export/Import (Disaster Recovery ZIP)

## Objective

Provide Owner-authenticated ZIP export/import of every configuration ProxyCore needs to fully restore on the same or a fresh host, while keeping the on-disk layout JSON-friendly so the future Postgres → SQLite migration does not require re-inventing the format.

Scope is intentionally narrower than "entire filesystem": runtime-only tables, candidate staging, and CoreDNS/Nginx rendered output are excluded because they regenerate deterministically from the configuration on every apply.

## Product Decisions

- **Scope of an export** (validated against the user's "toda la configuración + certificados + contraseñas"):
  - DB tables in JSON, one file per table under `db/`. **Config tables** (users, zones, dns_records, resolver_pools, forwarding_rules, stream_routes, secrets, internal_ca, internal_ca_enrollment_state, certificates, provider_connections, config_revisions, installation_settings, installation_identity, cluster_keys, node_state). **Runtime-only and history tables excluded**: sessions, audit_events, apply_jobs, applied_snapshots, standalone_archives (both have FKs to `apply_jobs` and other runtime tables), enrollment_attempts, enrollment_tokens, node_credentials, enrolled_nodes, enrollment_grants, node_snapshot_acks, sync_attempts, health_observations, operational_artifacts.
  - `.env` (`env/env`) — includes `PROXYCORE_MASTER_KEY_BASE64`. Exporting it is required for true disaster recovery; secrets in `secrets.ciphertext` are AES-256-GCM under this key and are unreadable without it.
  - Materialized certificates from the latest applied candidate (`certs/<certificateId>.crt` and `.key` under `candidates/<appliedRevision>/nginx/certs/`). The legacy `certificates` volume is unused; the staging candidate root is the source of truth for what Nginx serves today.
  - `manifest.json` (format version, install identity, checksums, counts, createdAt, exporter version).
- **Master key handling** (user-confirmed):
  - Export embeds `.env` exactly as deployed so `PROXYCORE_MASTER_KEY_BASE64` travels with the bundle.
  - Import refuses to run when the running master key does not match the one in `env/env` (fail-closed, surfaces a clear error). Operator can re-export with the correct master key, or rotate first via the existing secret-rotation path.
  - The ZIP itself is **unencrypted by default**. The Owner may optionally wrap the ZIP with a passphrase using AES-256 (PBKDF2-HMAC-SHA256 → AES-256-GCM); passphrase presence is recorded in the manifest as `encryption: { kdf, params, salt, verificationTag }` so the importer can detect it without leaking the passphrase.
- **Import semantics**:
  - **Dry-run first, always**: the import endpoint returns a structured preview of what would change (table counts to truncate, .env diff, cert files to overwrite) without mutating anything.
  - **Destructive restore**: import truncates the config tables inside one transaction and reinserts from the bundle; existing rows in those tables are lost. Runtime tables (sessions, audit_events, apply_jobs, etc.) are untouched.
  - **Post-import apply**: after a successful restore the API triggers `POST /api/apply` so Nginx/CoreDNS pick up the restored desired state automatically. If apply fails the import is rolled back.
  - Re-import of the same bundle is idempotent.
- **Authorization & UX**:
  - Endpoints are **Owner-only**. Requests require an active Owner session.
  - Two REST endpoints: `POST /api/backup/export` (returns a streamed ZIP with `Content-Disposition: attachment`) and `POST /api/backup/import?dry_run=true|false` (accepts multipart upload of the ZIP + optional passphrase).
  - Implementation runs **server-side in the Go API** (Go already owns DB access and the apply trigger; Bun/worker never touches secrets directly).
  - ZIP never includes the Owner session cookie or live database connection strings of the target host.
- **Future SQLite migration** (user-stated reason for JSON-per-table):
  - The DB payload is a directory of one JSON file per table with stable, content-addressed filenames (`db/<table>.json`). Import code reads directory listings, not PostgreSQL-dump SQL. Migrating to SQLite means swapping the per-table reader/writer, not the manifest or the wire format.
  - No Postgres-specific types in JSON: timestamps → RFC 3339 strings; bytea → base64url; jsonb → parsed JSON; uuids → strings; bigints → JSON-safe numbers through 2^53, decimal strings above that.
  - Secret ciphertexts are exported as the existing `v1:iv:tag:ct` string verbatim; the importer calls the same `secrets.EncryptSecret`/`DecryptSecret` so envelope format is not duplicated.

## Constraints and Safety Invariants

- Import never trusts a bundle's `manifest.json` until it has verified every file's SHA-256 against the manifest's checksum map.
- Import refuses bundles whose `manifest.formatVersion` is newer than the running API can read.
- Import requires a running Postgres that the API can reach; failing health checks abort before truncate.
- Truncate and reinsert run inside a single transaction; partial failure leaves the database unchanged.
- Master-key mismatch is detected **before** truncate by decrypting the first secret ciphertext in the bundle with the running key and comparing.
- Passphrase is never logged, written to audit values, or echoed in errors.
- Export never reads `apply_jobs`, `audit_events`, `sessions`, `enrollment_attempts`, or any other runtime-only table.
- Export streams the ZIP; it never materializes the whole archive in memory.
- The `.env` written by import uses 0600 mode; the bundle is chmod-agnostic (no metadata preserved).
- Import triggers `POST /api/apply` exactly once on success and records a structured audit entry with `actor=owner:<id>, action=backup.import`.
- `database/sql`-style pgx must keep using prepared statements / parameter binding — no string concatenation of bundle content into SQL.

## Out of Scope (this feature)

- Incremental or differential backups (always full snapshot).
- Cross-cluster import (importing a bundle from installation A into installation B with different `installation_identity` is rejected with a clear error).
- UI button (runbooks + curl example are sufficient for the MVP).
- Restore of the standalone `coredns-zones`, `coredns-config`, `updater-bootstrap-state`, or `control-socket` volumes (Corefile regenerates from desired state; the others are runtime-only and excluded by design).
- Bundling of TLS keys for the embedded enrollment listener (regenerated by PNE-2B on bootstrap).
- Encryption of certs materializados inside the bundle (the bundle is already inside the host's blast radius once restored; per-file encryption would only hide content from operators who can already `docker exec`).

## Review Units

- [ ] **FB-1 — backupcore package + manifest schema**: pure-Go package `apps/api/internal/backupcore/` with `Manifest`, `FormatVersion` (currently `1`), JSON helpers (deterministic ordering), SHA-256 computation over a `[]NamedFile`, and a structural validator (unknown top-level keys rejected). Strict TDD: RED on schema misshape, GREEN on happy path, TRIANGULATE on extra/missing keys, wrong formatVersion, tampered entryChecksums.
  - Evidence: RED captured before implementation (`cd apps/api && go test ./internal/backupcore/...`) with undefined `NamedFile`, `ComputeEntryChecksums`, and `Manifest`; GREEN/Triangulation: 32 focused tests passed; `gofmt -d apps/api/internal/backupcore/` clean. Commit identity pending the parent transaction controller (delegated writer does not stage or commit).
- [ ] **FB-2 — DB-to-JSON exporter (config tables only)**: `apps/api/internal/backupcore/dbexport/` that takes a `*pgxpool.Pool`, lists the 16 config tables in FK-safe order derived from `information_schema` (not hardcoded), runs `SELECT * FROM <table>` with type coercion (timestamptz → RFC3339, bytea → base64url, jsonb → parsed JSON, uuid → string, bigint → JSON-safe number or decimal string, enum → string), and writes `<table>.json` to a provided writer. Strict TDD with disposable Postgres 17: RED on missing tables/types, GREEN on full export, TRIANGULATE on empty DB, large DB (10k rows), unicode, nullable jsonb, row with all-NULL fields.
  - Evidence: RED captured before implementation (`cd apps/api && go test ./internal/backupcore/dbexport/...`) with undefined `coerceValueForOID`, `coerceRows`, `ZipWriter`, `Exporter`, and `ExporterOptions`; GREEN/Triangulation: 28 focused tests passed with PostgreSQL-gated tests skipped because `PGX_TEST_DATABASE_URL` was unset; `gofmt -d apps/api/internal/backupcore/dbexport/` clean. Commit identity pending the parent transaction controller (delegated writer does not stage or commit).
- [ ] **FB-3 — Host snapshot reader (.env + applied certs)**: `apps/api/internal/backupcore/hostread/` that reads the host `.env` path (default `${PROXYCORE_HOST_HOME}/.env`, overridable via env), resolves the applied candidate revision by reading `candidates/.applied` or by asking the configuration store, and lists `*.crt`/`*.key` files under `candidates/<revision>/nginx/certs/`. Strict TDD with a tempdir fixture.
  - Evidence: RED captured before implementation (`cd apps/api && go test ./internal/backupcore/hostread/...`) failed at package build with undefined `Config`, `New`, and `ErrNoAppliedRevision`; GREEN/Triangulation: 12 focused test cases passed with PostgreSQL-gated coverage skipped because `PGX_TEST_DATABASE_URL` and `DATABASE_URL` were unset; `gofmt -d` clean. Commit identity pending the parent transaction controller (delegated writer does not stage or commit).
- [ ] **FB-4 — ZIP packager + optional passphrase encryption**: `apps/api/internal/backupcore/zipwriter/` using Go stdlib `archive/zip` for packaging and a small `crypto/aes` + `crypto/pbkdf2` wrapper for optional AES-256-GCM passphrase encryption (when passphrase present the entire ZIP bytes are wrapped; otherwise ZIP is raw). Writes a streamed archive (no full in-memory copy). Strict TDD: round-trip with and without passphrase, corrupt passphrase detection, oversized file rejection (>2 GB per file).
  - Evidence: RED captured before implementation (`cd apps/api && go test ./internal/backupcore/...`) failed with absent zipwriter/zipextract packages and undefined `EscapesBundleRoot`; GREEN: focused ZIP tests passed (45 tests), full `cd apps/api && go test ./internal/backupcore/...` passed (167 tests), and `gofmt -d apps/api/internal/backupcore/` was clean. TRIANGULATE: raw/encrypted empty and multi-entry round-trips, 64 KiB chunk boundary, path sentinels and duplicates, exact aggregate boundary, unknown compression, oversized claims without opening, wrong passphrase, tampered/truncated envelopes, unsupported version/KDF, and central-directory path rejection are covered. Commit identity pending the parent transaction controller (delegated writer does not stage or commit).
- [ ] **FB-5 — REST endpoints (export, dry-run import, confirm import)**: `apps/api/internal/httpserver/backup_handlers.go` with `POST /api/backup/export` and `POST /api/backup/import?dry_run=…`. Owner-only. Streams export; multipart-decodes import; surfaces manifest preview on dry-run. Tests cover Owner-only (Operator gets 403), bad ZIP (400), master-key mismatch (409 with code `master_key_mismatch`), passphrase required (401).
  - Evidence: RED captured before implementation (`cd apps/api && go test -count=1 -v ./internal/httpserver/... -run TestBackup`) failed at package build on undefined `ImportReport`, `BackupExporter`, `BackupImporter`, `WithBackup`, and `TablePreview`; GREEN: focused backup handler tests and `cd apps/api && go test ./internal/backupcore/... ./internal/httpserver/...` passed; TRIANGULATE: anonymous/operator authorization, missing services, streamed export success and pre/post-header failures, passphrase decoding and non-TLS warning, dry-run query precedence, multipart validation, passphrase/master-key/archive/version/path/body-size error mappings, and passphrase redaction are covered. Target files are gofmt-clean; the full directory check still reports pre-existing formatting in `auth_test.go`. Commit identity pending the parent transaction controller (delegated writer does not stage or commit).
- [ ] **FB-6 — Import engine**: `apps/api/internal/backupcore/dbimport/` reads manifest, verifies checksums, verifies master key against the first secrets row, truncates config tables inside one transaction, reinserts in FK-safe order, writes `env/env`, restores certs to `candidates/<newRevision>/nginx/certs/`, and on success calls the apply orchestrator. Strict TDD with disposable Postgres: RED on tampered checksum, GREEN on round-trip, TRIANGULATE on partial failure (transaction rollback), master-key mismatch (no DB write), concurrent import (lock guard).
- [ ] **FB-7 — Integration: full export → import round-trip**: disposable Postgres 17, populated schema and data, real Go API process on `:0`, end-to-end test that exports, wipes DB, reimports, asserts every config table equals the pre-export snapshot byte-for-byte (modulo JSON serialization). Cover passphrase round-trip as a separate case.
- [ ] **FB-8 — Runbook + curl examples + audit event**: `docs/runbooks/backup-restore.md` (Owner checklist, passphrase guidance, restore-on-fresh-host recipe), audit event `backup.import` recorded with actor and bundle SHA-256 prefix. No new env vars.

## Build Order & Commit Discipline

Order: FB-1 → FB-2 → FB-3 → FB-4 → (FB-5 ‖ FB-6) → FB-7 → FB-8. FB-5 and FB-6 share the handler/import boundary but stay file-disjoint.

Each unit lands as a separate work-unit commit on this branch. Each commit:
- Changes fewer than 400 net lines outside test data.
- Keeps `cd apps/api && go test ./...` green.
- Keeps `bun run typecheck`, `bun run test`, and `git diff --check` green.
- Updates this task file with its commit identity and evidence under the unit's bullet.

Push, PR, and merge are user decisions; this task does not push or open a PR without explicit approval.

## Verification Evidence

(populated as each unit lands)

- FB-1: RED — pre-implementation focused test run failed at package build because the requested package symbols were undefined. GREEN — focused package tests passed (32 tests). TRIANGULATE — extra and missing top-level keys, wrong `formatVersion`, duplicate and malformed `entryChecksums`, escaping paths, encryption metadata, input ordering, and unreadable files are covered.
- FB-2: RED — package build failed before implementation on undefined exporter, writer, and coercion symbols. GREEN — `cd apps/api && go test ./internal/backupcore/... ./internal/backupcore/dbexport/...` passed (60 tests). TRIANGULATE — unit coverage includes every registered OID, NULL and empty bytea, JSON recursion, Unicode, bigint JSON-safe number/decimal-string boundaries, missing tables, FK cycles, empty output, all-NULL rows, and the 10k-row PostgreSQL-gated streaming case. Disposable PostgreSQL verification was not run locally because `PGX_TEST_DATABASE_URL` was unset.
- Hardening H1-H9: RED — the new bigint, encryption, Windows-drive, DB-table, checksum-reader, and PostgreSQL ordering cases exposed the verify gaps; existing character-type and non-hex-checksum coverage passed immediately. GREEN — `cd apps/api && go test -count=1 ./internal/backupcore/...` passed (90 tests), `cd apps/api && go test -count=1 -race ./internal/backupcore/...` passed (90 tests), and disposable `postgres:17-alpine` runs passed for both `go test -count=1 ./internal/backupcore/dbexport -v` and `go test -count=1 -race ./internal/backupcore/dbexport -v`. TRIANGULATE — H1 safe/exact/large signed boundaries; H2 nil/valid and zero, negative, malformed, and wrong-length metadata; H3 relative, nested, parent, backslash, and Windows-drive paths; H4 first DB-table offender; H7 all configured character OIDs and non-hex checksums; H8 normal, nil-reader, and reader-plus-error opener paths. The H9 FIFO-ready-queue correction keeps independent tables before newly-ready dependents. Commit identity pending the parent transaction controller (delegated writer does not stage or commit).
- FB-3: RED — the pre-implementation hostread package build failed on the undefined reader/config/sentinel symbols. GREEN — `cd apps/api && go test ./internal/backupcore/hostread/...` passed (12 focused test cases; PostgreSQL test skipped because both database URL variables were unset). TRIANGULATE — path defaults, missing `.env` PathError, empty applied-revision sentinel, missing cert directory, non-directory cert path, traversal and symlink escapes, case-insensitive extensions, preserved extensions, and nested-directory exclusion are covered; `gofmt -d` is clean.
- FB-4: RED — the pre-implementation package run failed with absent zipwriter/zipextract packages and undefined `EscapesBundleRoot`. GREEN — focused `go test -v ./internal/backupcore/zipwriter/... ./internal/backupcore/zipextract/...` passed (45 tests); full backupcore tests passed (167 tests); `gofmt -d` is clean. TRIANGULATE — envelope corruption, passphrase mismatch without payload leakage, raw/encrypted round-trips including the 64 KiB chunk boundary, path and size sentinels, aggregate limits, and central-directory validation are covered. Commit identity pending the parent transaction controller (delegated writer does not stage or commit).
- FB-5: RED — focused tests failed before implementation because the HTTP boundary interfaces, report types, and `WithBackup` option were undefined. GREEN — `cd apps/api && go test -v ./internal/httpserver/... -run TestBackup` and `cd apps/api && go test ./internal/backupcore/... ./internal/httpserver/...` passed. TRIANGULATE — authorization, service absence, export streaming/error timing, encoded passphrases, dry-run source-of-truth, multipart/body-size failures, import sentinel mappings, and secret redaction are covered. `gofmt -d apps/api/internal/httpserver/` remains blocked by pre-existing formatting in `auth_test.go`; the changed files are clean.
