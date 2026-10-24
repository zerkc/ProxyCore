// Drives the real Bun SQLite job and revision stores against a shared database
// file for Go interop tests. Usage: bun stores.bun.ts <db> <operation> [json].
// Prints the operation result as one JSON line (Dates serialize as ISO strings).
import { openSQLite } from "../../../../../packages/db/src/sqlite-foundation/open.js";
import { createSQLiteJobStore } from "../../../../../packages/db/src/sqlite-foundation/job-store.js";
import { createSQLiteRevisionStore } from "../../../../../packages/db/src/sqlite-foundation/revision-store.js";

const [path, operation, raw] = process.argv.slice(2);
const input = raw === undefined ? {} : JSON.parse(raw);
const db = openSQLite(path);
const jobs = createSQLiteJobStore(db);
const revisions = createSQLiteRevisionStore(db);

const dates = ["claimedAt", "startedAt", "finishedAt"];
function revive(value: Record<string, unknown>): Record<string, unknown> {
  const copy = { ...value };
  for (const key of dates) if (typeof copy[key] === "string") copy[key] = new Date(copy[key] as string);
  return copy;
}

try {
  let result: unknown;
  switch (operation) {
    case "get": result = await jobs.get(input.id); break;
    case "list": result = await jobs.list(); break;
    case "claimNext": result = await jobs.claimNext(input.target); break;
    case "recoverStale": result = await jobs.recoverStale(input.leaseMs, input.now ? new Date(input.now) : undefined); break;
    case "update": result = await jobs.update(input.id, revive(input.patch)); break;
    case "enqueue": result = await jobs.enqueue(input); break;
    case "enqueueIfNotActive": result = await jobs.enqueueIfNotActive(input); break;
    case "getRevision": result = await revisions.get(input.id); break;
    case "latestRevision": result = await revisions.latest(); break;
    case "markApplied": result = await revisions.markApplied(input.id, input.appliedAt ? new Date(input.appliedAt) : undefined); break;
    default: throw new Error(`unknown operation ${operation}`);
  }
  console.log(JSON.stringify(result ?? null));
} finally {
  db.close();
}
