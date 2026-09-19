import { describe, expect, it } from "vitest";
import {
  getDashboardNavigation,
  getDashboardSurface,
  getIdentityTuple,
  getTopologyInspectorRows,
} from "./DashboardShell";
import { dashboardNav, resolveDashboardNav } from "./nav";

describe("dashboard continuity navigation", () => {
  it("routes authenticated dashboard navigation to Continuity", () => {
    expect(dashboardNav).toContainEqual({
      href: "/dashboard/continuity",
      id: "continuity",
      label: "continuity",
      title: "enrollment hostnames",
      icon: "continuity",
    });
    expect(resolveDashboardNav("/dashboard/continuity").id).toBe("continuity");
  });
});

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

describe("dashboard topology surface routing", () => {
  it.each([
    ["node", "node-only"],
    ["primary-with-nodes", "ordinary"],
    ["standalone-primary", "ordinary"],
    ["primary", "ordinary"],
    ["stale-primary", "error"],
    ["stale-generation", "error"],
  ] as const)("maps %s to the %s surface", (role, expected) => {
    expect(getDashboardSurface({ role } as never)).toBe(expected);
  });

  it("fails closed when the topology identity is unloaded or marked stale", () => {
    expect(getDashboardSurface(undefined)).toBe("error");
    expect(getDashboardSurface({ role: "primary", stalePrimary: true })).toBe("error");
  });

  it("hides every ordinary route in NODE-only mode", () => {
    expect(getDashboardNavigation("node-only").map((item) => item.id)).toEqual([
      "continuity",
    ]);
    expect(getDashboardNavigation("node-only").some((item) => item.id === "dns")).toBe(false);
  });

  it("changes the reload tuple for role, installation, node, cluster key, and generation", () => {
    const identity = {
      role: "primary",
      installationId: "installation-a",
      nodeId: "node-a",
      clusterKeyId: "cluster-a",
      leadershipGeneration: 1,
    };
    expect(getIdentityTuple(identity)).toBe(
      "primary|installation-a|node-a|cluster-a|1",
    );
    for (const [field, value] of [
      ["role", "node"],
      ["installationId", "installation-b"],
      ["nodeId", "node-b"],
      ["clusterKeyId", "cluster-b"],
      ["leadershipGeneration", 2],
    ] as const) {
      expect(getIdentityTuple({ ...identity, [field]: value })).not.toBe(
        getIdentityTuple(identity),
      );
    }
  });
});
