import { describe, expect, it } from "vitest";
import { getTopologyInspectorRows } from "./DashboardShell";

describe("topology inspector identity", () => {
  it("surfaces redacted role, identifiers, generations, and write state", () => {
    expect(
      getTopologyInspectorRows({
        installationId: "installation-123",
        nodeId: "node-456",
        role: "node",
        leadershipGeneration: 7,
        latestKnownGeneration: 9,
        stalePrimary: false,
        writable: false,
      }),
    ).toEqual([
      { label: "role", value: "node" },
      { label: "installation ID", value: "installation-123" },
      { label: "node ID", value: "node-456" },
      { label: "generation", value: "7 / 9" },
      { label: "writable", value: "no" },
      { label: "stale primary", value: "no" },
    ]);
  });
});
