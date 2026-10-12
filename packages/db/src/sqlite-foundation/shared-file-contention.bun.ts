import { existsSync } from "node:fs";
import { openSQLite } from "./open.js";
import { claimNextSQLiteJob } from "./job-claim.js";
import { updateSQLiteJobRecord } from "./job-update-record.js";
import { SQLITE_JOB_CURSOR_SQL } from "./job-wakeup.js";

// Worker-side contention participant for the Go shared-file gate. It uses the
// worker's real claim (BEGIN IMMEDIATE via SQLiteWriter) and status update
// (autocommit UPDATE via updateSQLiteJobRecord) paths, plus the wakeup cursor read.
// It drains queued jobs until the Go side creates the stop file after all writers
// finished, then prints one JSON summary line on stdout. Any SQLite error exits non-zero.
const [path, stopPath] = process.argv.slice(2);
if (!path || !stopPath) {
  throw new Error("usage: shared-file-contention.bun.ts <database> <stop-file>");
}

const db = openSQLite(path);
const summary = { claimed: 0, applied: 0, cursorReads: 0 };
try {
  for (;;) {
    // Read the stop marker before claiming: a job committed before the marker is
    // visible to the claim that follows, so an empty claim after it is final.
    const stop = existsSync(stopPath);
    db.query<{ cursor: string }, []>(SQLITE_JOB_CURSOR_SQL).get();
    summary.cursorReads++;
    const job = claimNextSQLiteJob(db);
    if (job === undefined) {
      if (stop) break;
      await new Promise((resolve) => setTimeout(resolve, 2));
      continue;
    }
    summary.claimed++;
    await updateSQLiteJobRecord(db, job.id, {
      status: "applied",
      healthOutput: { observedBy: "worker", attempt: summary.claimed },
      finishedAt: new Date(),
    });
    summary.applied++;
  }
} finally {
  db.close();
}
console.log(JSON.stringify(summary));
