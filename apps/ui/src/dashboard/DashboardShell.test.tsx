import { createElement } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  NodeOnlyBody,
  getDashboardNavigation,
  getDashboardSurface,
  getIdentityTuple,
  getTopologyInspectorRows,
} from "./DashboardShell";
import { dashboardNav, resolveDashboardNav } from "./nav";
import { deferred, jsonResponse, mount, queueFetch } from "./testDom";

vi.mock("./NodeContinuityView", () => ({
  NodeContinuityView: () => createElement("div", { "data-testid": "node-continuity" }),
}));
vi.mock("./TopologyConversionPanel", () => ({
  TopologyConversionPanel: () => createElement("div", { "data-testid": "topology-conversion" }),
}));

afterEach(() => vi.restoreAllMocks());

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

  it("exposes backup navigation and hides it on node-only surfaces", () => {
    expect(dashboardNav).toContainEqual({
      href: "/dashboard/backup",
      id: "backup",
      label: "backup",
      title: "backup",
      icon: "backup",
    });
    expect(resolveDashboardNav("/dashboard/backup").id).toBe("backup");
    expect(resolveDashboardNav("/dashboard/nodes").id).toBe("nodes");
    expect(resolveDashboardNav("/dashboard/continuity").id).toBe("continuity");
    expect(
      getDashboardNavigation("ordinary").some((item) => item.id === "backup"),
    ).toBe(true);
    expect(
      getDashboardNavigation("node-only").some((item) => item.id === "backup"),
    ).toBe(false);
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

  it("lists the nodes nav entry only for primary roles on the ordinary surface", () => {
    const has = (surface: "ordinary" | "node-only", role?: "primary" | "primary-with-nodes" | "standalone-primary" | "node") =>
      getDashboardNavigation(surface, role).some((item) => item.id === "nodes");
    expect(has("ordinary", "primary")).toBe(true);
    expect(has("ordinary", "primary-with-nodes")).toBe(true);
    expect(has("ordinary", "standalone-primary")).toBe(false);
    expect(has("ordinary")).toBe(false);
    expect(has("node-only", "node")).toBe(false);
  });
});

describe("node-only enrollment gate", () => {
  const syncStatus = { role: "node", bound: true, outdated: false, consecutiveFailures: 0, acknowledged: true };
  const has = (container: { findAll: (fn: (e: { getAttribute: (n: string) => string | null }) => boolean) => unknown[] }, id: string) =>
    container.findAll((element) => element.getAttribute("data-testid") === id).length > 0;
  const body = () => createElement(NodeOnlyBody, { identity: { role: "node" } as never, identityTuple: "node|a", onConverted: () => undefined });

  it("hides the enrollment form on a linked node and keeps sync status and conversion", async () => {
    queueFetch(jsonResponse(syncStatus));
    const container = await mount(body());
    expect(has(container, "node-continuity")).toBe(false);
    expect(has(container, "node-sync-status")).toBe(true);
    expect(has(container, "topology-conversion")).toBe(true);
  });

  it("shows the enrollment form when the node is not bound", async () => {
    queueFetch(jsonResponse({ ...syncStatus, bound: false }));
    const container = await mount(body());
    expect(has(container, "node-continuity")).toBe(true);
    expect(has(container, "node-sync-status")).toBe(true);
  });

  it("shows the enrollment form when the PRIMARY unlinked the node", async () => {
    queueFetch(jsonResponse({ ...syncStatus, unlinked: true, unlinkedAt: "2026-10-14T10:00:00Z" }));
    const container = await mount(body());
    expect(has(container, "node-continuity")).toBe(true);
  });

  it("does not render the form while the status is loading", async () => {
    const pending = deferred<Response>();
    queueFetch(pending.promise);
    const container = await mount(body());
    expect(has(container, "node-continuity")).toBe(false);
    expect(has(container, "topology-conversion")).toBe(true);
  });

  it("keeps showing the enrollment form when the status cannot be loaded", async () => {
    queueFetch(jsonResponse({ error: "boom" }, 500));
    const container = await mount(body());
    expect(has(container, "node-continuity")).toBe(true);
  });
});
