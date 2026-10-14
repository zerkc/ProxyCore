import { mkdtemp, rm } from "node:fs/promises";
import { readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

// bun:sqlite exists only under Bun; the root vitest (node) run skips this file.
const underBun = "Bun" in globalThis;
const MASTER_KEY = Buffer.alloc(32, 7).toString("base64");

let dir: string;
beforeEach(async () => {
  dir = await mkdtemp(join(tmpdir(), "worker-sqlite-"));
});
afterEach(async () => {
  await rm(dir, { recursive: true, force: true });
});

async function migrate(path: string): Promise<void> {
  const { openSQLite } =
    await import("../../../packages/db/src/sqlite-foundation/open");
  const { migrateSQLite } =
    await import("../../../packages/db/src/sqlite-foundation/migrate");
  const db = openSQLite(path);
  try {
    migrateSQLite(db);
  } finally {
    db.close();
  }
}

const fastWait = { schemaWaitMs: 300, schemaPollMs: 10 };

describe.skipIf(!underBun)("createWorkerRuntime", () => {
  it("keeps the expected schema version aligned with the Go registry", async () => {
    const { EXPECTED_SQLITE_SCHEMA_VERSION } = await import("./sqlite-runtime");
    const registry = JSON.parse(
      readFileSync(
        fileURLToPath(
          new URL(
            "../../api/internal/sqlitedb/migrations/registry.json",
            import.meta.url,
          ),
        ),
        "utf8",
      ),
    ) as unknown[];
    expect(EXPECTED_SQLITE_SCHEMA_VERSION).toBe(registry.length);
  });

  it("waits for the file and schema, then starts without migrating", async () => {
    const { createWorkerRuntime } = await import("./sqlite-runtime");
    const path = join(dir, "late.db");
    const logs: string[] = [];
    setTimeout(() => void migrate(path), 60);
    const runtime = await createWorkerRuntime(
      { sqlitePath: path },
      { ...fastWait, schemaWaitMs: 5_000, log: (m) => logs.push(m) },
    );
    try {
      expect(logs.some((m) => /waiting for sqlite schema/i.test(m))).toBe(true);
      expect(await runtime.jobs.list()).toEqual([]);
    } finally {
      await runtime.close();
    }
  });

  it("does not create the database file while waiting", async () => {
    const { createWorkerRuntime } = await import("./sqlite-runtime");
    const { existsSync } = await import("node:fs");
    const path = join(dir, "never.db");
    await expect(
      createWorkerRuntime({ sqlitePath: path }, fastWait),
    ).rejects.toThrow(/schema.*not ready/i);
    expect(existsSync(path)).toBe(false);
  });

  it("fails after the bound when the schema stays unmigrated", async () => {
    const { openSQLite } =
      await import("../../../packages/db/src/sqlite-foundation/open");
    const { createWorkerRuntime } = await import("./sqlite-runtime");
    const path = join(dir, "empty.db");
    openSQLite(path).close();
    await expect(
      createWorkerRuntime({ sqlitePath: path }, fastWait),
    ).rejects.toThrow(/version 7.*not ready|not ready.*version 7/is);
  });

  it("fails fast when the schema is newer than the worker supports", async () => {
    const { createWorkerRuntime } = await import("./sqlite-runtime");
    const path = join(dir, "newer.db");
    await migrate(path);
    await expect(
      createWorkerRuntime(
        { sqlitePath: path },
        { ...fastWait, expectedSchemaVersion: 1 },
      ),
    ).rejects.toThrow(/newer/i);
  });

  it("omits the secret store without a master key and provides it with one", async () => {
    const { createWorkerRuntime } = await import("./sqlite-runtime");
    const path = join(dir, "secrets.db");
    await migrate(path);
    const without = await createWorkerRuntime({ sqlitePath: path }, fastWait);
    try {
      expect(without.secretStore).toBeUndefined();
    } finally {
      await without.close();
    }
    const withKey = await createWorkerRuntime(
      { sqlitePath: path, masterKeyBase64: MASTER_KEY },
      fastWait,
    );
    try {
      const id = await withKey.secretStore!.put("test", "plain");
      expect(await withKey.secretStore!.get(id)).toBe("plain");
    } finally {
      await withKey.close();
    }
  });

  it("fires the wakeup when a job is enqueued and closes cleanly", async () => {
    const { createWorkerRuntime } = await import("./sqlite-runtime");
    const path = join(dir, "wake.db");
    await migrate(path);
    const runtime = await createWorkerRuntime(
      { sqlitePath: path },
      { ...fastWait, wakeupPollMs: 10 },
    );
    const revision = await runtime.revisions.create({ hello: "world" });
    await runtime.wakeup.wait(1_000); // priming notification
    const started = Date.now();
    await runtime.jobs.enqueue({
      revisionId: revision.id,
      target: "combined",
      correlationId: "c-1",
    });
    await runtime.wakeup.wait(2_000);
    expect(Date.now() - started).toBeLessThan(1_500);
    await runtime.close();
    await runtime.close(); // idempotent
    await expect(runtime.jobs.list()).rejects.toThrow();
  });
});
