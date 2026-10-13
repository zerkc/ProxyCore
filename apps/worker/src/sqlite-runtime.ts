import type { Database } from "bun:sqlite";
import { existsSync } from "node:fs";
import type { SecretStore } from "@proxycore/certificates";
import type { JobStore, RevisionStore } from "@proxycore/db";
import { openSQLite } from "@proxycore/db/sqlite-foundation/open";
import { createSQLiteJobStore } from "@proxycore/db/sqlite-foundation/job-store";
import { createSQLiteRevisionStore } from "@proxycore/db/sqlite-foundation/revision-store";
import { createSQLiteSecretStore } from "@proxycore/db/sqlite-foundation/secret-store";
import {
  createSQLiteJobWakeup,
  SQLITE_JOB_CURSOR_SQL,
} from "@proxycore/db/sqlite-foundation/job-wakeup";
import { WorkerWakeup } from "./poller";

/** Last migration in apps/api/internal/sqlitedb/migrations/registry.json. The Go API
 * owns migration; a test keeps this aligned with the registry (the worker image does
 * not ship apps/api). */
export const EXPECTED_SQLITE_SCHEMA_VERSION = 6;

export type WorkerRuntimeConfig = {
  sqlitePath: string;
  masterKeyBase64?: string;
};

export type WorkerRuntimeOptions = {
  expectedSchemaVersion?: number;
  /** Total time to wait for the Go API to create and migrate the database. */
  schemaWaitMs?: number;
  schemaPollMs?: number;
  wakeupPollMs?: number;
  log?: (message: string) => void;
  sleep?: (milliseconds: number) => Promise<void>;
};

export type WorkerRuntime = {
  jobs: JobStore;
  revisions: RevisionStore;
  secretStore?: SecretStore;
  wakeup: WorkerWakeup;
  /** Stops the wakeup poller and closes both SQLite handles. Idempotent. */
  close(): Promise<void>;
};

/** Composes the worker on SQLite only. It never migrates: it waits (bounded, logged)
 * for the Go API to create the file and reach the expected schema version. */
export async function createWorkerRuntime(
  config: WorkerRuntimeConfig,
  options: WorkerRuntimeOptions = {},
): Promise<WorkerRuntime> {
  const log = options.log ?? (() => {});
  await waitForSchema(config.sqlitePath, options, log);

  const writeDb = openSQLite(config.sqlitePath);
  let readDb: Database | undefined;
  try {
    readDb = openSQLite(config.sqlitePath);
    const cursorDb = readDb;
    const wakeup = new WorkerWakeup();
    const controller = new AbortController();
    const polling = createSQLiteJobWakeup().run({
      signal: controller.signal,
      pollIntervalMs: options.wakeupPollMs,
      onJob: () => wakeup.notify(),
      readCursor: () =>
        cursorDb.query<{ cursor: string }, []>(SQLITE_JOB_CURSOR_SQL).get()
          ?.cursor,
      onError: (error) =>
        log(
          `worker job wakeup error: ${error instanceof Error ? error.message : String(error)}`,
        ),
    });
    let closed: Promise<void> | undefined;
    return {
      jobs: createSQLiteJobStore(writeDb),
      revisions: createSQLiteRevisionStore(writeDb),
      secretStore: config.masterKeyBase64
        ? createSQLiteSecretStore(writeDb, config.masterKeyBase64)
        : undefined,
      wakeup,
      close() {
        closed ??= (async () => {
          controller.abort();
          wakeup.notify();
          await polling;
          cursorDb.close();
          writeDb.close();
        })();
        return closed;
      },
    };
  } catch (error) {
    readDb?.close();
    writeDb.close();
    throw error;
  }
}

async function waitForSchema(
  path: string,
  options: WorkerRuntimeOptions,
  log: (message: string) => void,
): Promise<void> {
  const expected =
    options.expectedSchemaVersion ?? EXPECTED_SQLITE_SCHEMA_VERSION;
  const waitMs = options.schemaWaitMs ?? 60_000;
  const pollMs = options.schemaPollMs ?? 1_000;
  const sleep =
    options.sleep ?? ((ms) => new Promise<void>((r) => setTimeout(r, ms)));
  const deadline = Date.now() + waitMs;
  let reason: string;
  for (;;) {
    const state = inspectSchema(path);
    if (state.kind === "ready" || state.kind === "unready") {
      if (state.kind === "ready" && state.version === expected) return;
      if (state.kind === "ready" && state.version > expected) {
        throw new Error(
          `sqlite schema version ${state.version} is newer than the version ${expected} this worker supports`,
        );
      }
      reason =
        state.kind === "ready" ? `at version ${state.version}` : state.reason;
    } else {
      reason = state.reason;
    }
    if (Date.now() >= deadline) break;
    log(`waiting for sqlite schema version ${expected} at ${path}: ${reason}`);
    await sleep(pollMs);
  }
  throw new Error(
    `sqlite schema not ready after ${waitMs}ms (expected version ${expected}, ${reason}); the Go API owns migration`,
  );
}

type SchemaState =
  | { kind: "ready"; version: number }
  | { kind: "unready"; reason: string }
  | { kind: "missing"; reason: string };

function inspectSchema(path: string): SchemaState {
  // Opening would create an empty database and race the Go API's migration.
  if (!existsSync(path))
    return { kind: "missing", reason: "database file does not exist" };
  let db: Database | undefined;
  try {
    db = openSQLite(path);
    const row = db
      .query<{ version: bigint | null }, []>(
        "SELECT max(version) AS version FROM main.proxycore_schema_migrations",
      )
      .get();
    return row?.version == null
      ? { kind: "unready", reason: "no migrations applied" }
      : { kind: "ready", version: Number(row.version) };
  } catch (error) {
    return {
      kind: "unready",
      reason: error instanceof Error ? error.message : String(error),
    };
  } finally {
    db?.close();
  }
}
