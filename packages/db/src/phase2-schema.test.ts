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
    expect(phase2PersistenceContract.installationSettingsColumns).toEqual([
      "enrollment_hostnames",
    ]);
    expect(phase2PersistenceContract.enrollmentGrantColumns).toEqual([
      "verified_preview_digest",
    ]);
    expect(phase2PersistenceContract.crossInstallationAttemptColumns).toEqual([
      "enrollment_tokens.consumed_by_attempt_id",
      "enrollment_grants.attempt_id",
      "enrolled_nodes.created_by_attempt_id",
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
    const enrollmentMigration = readFileSync(
      new URL("../migrations/0005_enrollment_hostnames.sql", import.meta.url),
      "utf8",
    );
    const tlsIdentityMigration = readFileSync(
      new URL("../migrations/0006_enrollment_tls_identity.sql", import.meta.url),
      "utf8",
    );
    const grantDigestMigration = readFileSync(
      new URL(
        "../migrations/0007_enrollment_grant_preview_digest.sql",
        import.meta.url,
      ),
      "utf8",
    );
    const journal = JSON.parse(
      readFileSync(new URL("../migrations/meta/_journal.json", import.meta.url), "utf8"),
    ) as { entries: Array<{ idx: number; tag: string }> };

    expect(journal.entries.at(-1)).toMatchObject({
      idx: 7,
      tag: "0007_enrollment_grant_preview_digest",
    });
    expect(grantDigestMigration).toContain(
      'ALTER TABLE "enrollment_grants" ADD COLUMN IF NOT EXISTS "verified_preview_digest" text',
    );
    for (const constraint of [
      '"enrollment_tokens_consumed_by_attempt_id_enrollment_attempts_id_fk"',
      '"enrollment_tokens_consumed_by_attempt_id_fkey"',
      '"enrollment_grants_attempt_id_enrollment_attempts_id_fk"',
      '"enrollment_grants_attempt_id_fkey"',
      '"enrolled_nodes_created_by_attempt_id_enrollment_attempts_id_fk"',
      '"enrolled_nodes_created_by_attempt_id_fkey"',
    ]) {
      expect(grantDigestMigration).toContain(
        `DROP CONSTRAINT IF EXISTS ${constraint}`,
      );
    }
    expect(tlsIdentityMigration).toContain(
      `CREATE TABLE IF NOT EXISTS "${phase2PersistenceContract.internalCaEnrollmentState}"`,
    );
    expect(tlsIdentityMigration).toContain('"established_at"');
    for (const table of Object.values(phase2PersistenceContract.tables)) {
      expect(migration).toContain(`CREATE TABLE "${table}"`);
    }
    for (const column of phase2PersistenceContract.nodeStateColumns) {
      expect(migration).toContain(`"${column}"`);
    }
    for (const column of phase2PersistenceContract.attributionColumns) {
      expect(migration).toContain(`"${column}"`);
    }
    for (const column of phase2PersistenceContract.installationSettingsColumns) {
      expect(enrollmentMigration).toContain(`"${column}"`);
    }
    for (const column of phase2PersistenceContract.internalCaColumns) {
      expect(tlsIdentityMigration).toContain(`"${column}"`);
    }
    expect(tlsIdentityMigration).toContain(
      '"internal_ca_enrollment_key_secret_id_secrets_id_fk"',
    );
    expect(migration).toContain('"enrollment_attempts_one_active_idx"');
    expect(migration).toContain('"enrollment_attempts_state_check"');
    expect(migration).toContain('"sync_attempts_trigger_check"');
    expect(migration).toContain('"config_revisions_source_check"');
    expect(migration).toContain('"applied_snapshots_status_check"');
  });
});
