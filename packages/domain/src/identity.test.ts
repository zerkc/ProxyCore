import { describe, expect, it } from "vitest";
import {
  InstallationID,
  LeadershipGeneration,
  NodeID,
  ReplicationVersion,
  ReplicationVersionV1,
  SnapshotVersion,
  SnapshotVersionV1,
  TopologyRole,
  TopologyRoleNode,
  TopologyRolePrimary,
  TopologyRolePrimaryWithNodes,
  TopologyRoleStalePrimary,
  TopologyRoleStandalone,
  isTopologyRole,
  newInstallationID,
  newNodeID,
} from "./identity";

describe("TopologyRole", () => {
  it("accepts the five known roles", () => {
    expect(isTopologyRole(TopologyRoleStandalone)).toBe(true);
    expect(isTopologyRole(TopologyRolePrimary)).toBe(true);
    expect(isTopologyRole(TopologyRolePrimaryWithNodes)).toBe(true);
    expect(isTopologyRole(TopologyRoleNode)).toBe(true);
    expect(isTopologyRole(TopologyRoleStalePrimary)).toBe(true);
  });

  it("rejects unknown role strings", () => {
    expect(isTopologyRole("")).toBe(false);
    expect(isTopologyRole("owner")).toBe(false);
    expect(isTopologyRole("PRIMARY")).toBe(false);
    expect(isTopologyRole("node-pending")).toBe(false);
    expect(isTopologyRole(undefined)).toBe(false);
    expect(isTopologyRole(null)).toBe(false);
    expect(isTopologyRole(42)).toBe(false);
  });

  it("exposes a stable string per role", () => {
    const roles: TopologyRole[] = [
      TopologyRoleStandalone,
      TopologyRolePrimary,
      TopologyRolePrimaryWithNodes,
      TopologyRoleNode,
      TopologyRoleStalePrimary,
    ];
    const unique = new Set(roles);
    expect(unique.size).toBe(5);
  });
});

describe("InstallationID", () => {
  it("generates a UUIDv4 each call", () => {
    for (let i = 0; i < 200; i++) {
      const id = newInstallationID();
      expect(id.isValid()).toBe(true);
    }
  });

  it("shows high uniqueness across 1000 calls", () => {
    const seen = new Set<string>();
    for (let i = 0; i < 1000; i++) {
      seen.add(newInstallationID().value);
    }
    expect(seen.size).toBeGreaterThanOrEqual(990);
  });

  it("accepts a real UUIDv4", () => {
    expect(new InstallationID("550e8400-e29b-41d4-a716-446655440000").isValid()).toBe(true);
  });

  it("rejects UUIDv1, malformed, and uppercase variants", () => {
    expect(new InstallationID("550e8400-e29b-11d4-a716-446655440000").isValid()).toBe(false);
    expect(new InstallationID("550e8400-e29b-41d4-a716").isValid()).toBe(false);
    expect(new InstallationID("550e8400-e29b-41d4-a716-446655440000-extra").isValid()).toBe(false);
    expect(new InstallationID("550E8400-E29B-41D4-A716-446655440000").isValid()).toBe(false);
    expect(new InstallationID("not-a-uuid").isValid()).toBe(false);
    expect(new InstallationID("").isValid()).toBe(false);
  });
});

describe("NodeID", () => {
  it("generates a UUIDv4 each call", () => {
    const id = newNodeID();
    expect(id.isValid()).toBe(true);
  });

  it("shares validation rules with InstallationID", () => {
    expect(new NodeID("550e8400-e29b-41d4-a716-446655440000").isValid()).toBe(true);
    expect(new NodeID("not-a-uuid").isValid()).toBe(false);
  });
});

describe("LeadershipGeneration", () => {
  it("increments by one without mutating the receiver", () => {
    const g = new LeadershipGeneration(10);
    const next = g.next();
    expect(next.value).toBe(11);
    expect(g.value).toBe(10);
  });

  it("handles the zero case", () => {
    const g = new LeadershipGeneration(0);
    expect(g.next().value).toBe(1);
  });

  it("throws on overflow", () => {
    // We construct the maximum value directly because LeadershipGeneration
    // does not allow constructing it through normal paths in tests.
    const max = new LeadershipGeneration(Number.MAX_SAFE_INTEGER);
    expect(() => max.next()).toThrow(/overflow/i);
  });

  it("compares correctly", () => {
    const a = new LeadershipGeneration(5);
    const b = new LeadershipGeneration(6);
    expect(a.isLessThan(b)).toBe(true);
    expect(b.isGreaterThan(a)).toBe(true);
    expect(a.equals(new LeadershipGeneration(5))).toBe(true);
  });
});

describe("SnapshotVersion", () => {
  it("rejects zero", () => {
    expect(new SnapshotVersion(0).isValid()).toBe(false);
  });

  it("accepts v1", () => {
    expect(SnapshotVersionV1.isValid()).toBe(true);
  });

  it("accepts higher values that this build does not yet define", () => {
    expect(new SnapshotVersion(2).isValid()).toBe(true);
  });
});

describe("ReplicationVersion", () => {
  it("rejects zero", () => {
    expect(new ReplicationVersion(0).isValid()).toBe(false);
  });

  it("accepts v1", () => {
    expect(ReplicationVersionV1.isValid()).toBe(true);
  });
});
