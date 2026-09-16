import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import { phase2PersistenceContract } from "./schema";

describe("phase 2 persistence contract", () => {
  it("describes every additive table and checked state", () => {
    expect(Object.keys(phase2PersistenceContract.tables)).toEqual([
      "enrollmentTokens",
      "enrolledNodes",
      "nodeCredentials",
      "enrollmentGrants",
      "nodeSnapshotAcks",
      "enrollmentAttempts",
      "syncAttempts",
      "standaloneArchives",
    ]);
    expect(phase2PersistenceContract.enrollmentAttemptStates).toContain(
      "initial-apply-pending",
    );
    expect(phase2PersistenceContract.syncTriggers).toEqual([
      "enrollment",
      "periodic",
      "manual",
      "restart",
    ]);
    expect(phase2PersistenceContract.sources).toEqual([
      "ordinary",
      "import",
      "sync",
    ]);
  });

  it("keeps the generated migration aligned with the contract", () => {
    const migration = readFileSync(
      new URL(
        "../migrations/0004_primary_node_continuity_phase_2.sql",
        import.meta.url,
      ),
      "utf8",
    );

    for (const table of Object.values(phase2PersistenceContract.tables)) {
      expect(migration).toContain(`CREATE TABLE "${table}"`);
    }
    for (const column of phase2PersistenceContract.nodeStateColumns) {
      expect(migration).toContain(`"${column}"`);
    }
    for (const column of phase2PersistenceContract.attributionColumns) {
      expect(migration).toContain(`"${column}"`);
    }
    expect(migration).toContain('"enrollment_attempts_one_active_idx"');
    expect(migration).toContain('"enrollment_attempts_state_check"');
    expect(migration).toContain('"sync_attempts_trigger_check"');
    expect(migration).toContain('"config_revisions_source_check"');
    expect(migration).toContain('"applied_snapshots_status_check"');
  });
});
