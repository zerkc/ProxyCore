import { describe, expect, it } from "vitest";
import {
  ClassificationError,
  Envelope,
  expectedHash,
  newEnvelope,
  validate,
  verifyHash,
} from "./replication";

function seal(env: Envelope): Envelope {
  return { ...env, contentHash: expectedHash(env) };
}

describe("replication envelope", () => {
  it("validates a freshly-sealed envelope", () => {
    const env = seal(newEnvelope());
    expect(() => validate(env)).not.toThrow();
  });

  it("rejects a missing replicated.configuration", () => {
    const env = seal({ ...newEnvelope(), replicated: { configuration: null, secrets: [], owners: [] } });
    expect(() => validate(env)).toThrow(ClassificationError);
  });

  it("rejects an invalid nodeLocal.role", () => {
    const env = seal({ ...newEnvelope(), nodeLocal: { ...newEnvelope().nodeLocal, role: "ghost" as never } });
    expect(() => validate(env)).toThrow(ClassificationError);
  });

  it("rejects a zero snapshotVersion", () => {
    const env = seal({ ...newEnvelope(), transient: { ...newEnvelope().transient, snapshotVersion: 0 } });
    expect(() => validate(env)).toThrow(/snapshotVersion/);
  });

  it("rejects a missing capturedAt", () => {
    const env = seal({ ...newEnvelope(), transient: { ...newEnvelope().transient, capturedAt: "" } });
    expect(() => validate(env)).toThrow(/capturedAt/);
  });

  it("content hash is stable across equivalent replicated payloads", () => {
    const a = seal(newEnvelope());
    const b = seal(newEnvelope());
    // Replicated payloads are equivalent (both empty objects) but transient
    // and node-local fields differ; the hash must remain stable.
    expect(a.contentHash).toBe(b.contentHash);
  });

  it("content hash is unchanged by transient and node-local mutation", () => {
    const env = newEnvelope();
    const before = expectedHash(env);
    env.transient.capturedAt = "2030-06-07T08:09:10.000Z";
    env.transient.sourcePrimaryId = "00000000-0000-4000-8000-000000000000";
    env.transient.leadershipGeneration = 99;
    env.nodeLocal.ingress = { ipv4: "198.51.100.5", ipv6: "2001:db8::5" };
    env.nodeLocal.role = "primary";
    expect(expectedHash(env)).toBe(before);
  });

  it("content hash changes when replicated payload mutates", () => {
    const env = newEnvelope();
    const before = expectedHash(env);
    env.replicated.secrets = [
      ...env.replicated.secrets,
      { id: "00000000-0000-4000-8000-000000000001", purpose: "tls-key", envelope: "v1.kek:AA:BB:CC" },
    ];
    expect(expectedHash(env)).not.toBe(before);
  });

  it("verifyHash detects a tampered replicated payload", () => {
    const env = seal(newEnvelope());
    env.replicated.owners = [
      ...env.replicated.owners,
      {
        userId: "00000000-0000-4000-8000-000000000002",
        username: "imposter",
        passwordHash: "scrypt$16384$8$1$injected$injected",
        role: "owner",
      },
    ];
    expect(() => verifyHash(env)).toThrow(/content hash mismatch/);
  });

  it("validate rejects a stale declared contentHash", () => {
    const env = seal(newEnvelope());
    env.contentHash = "deadbeef";
    expect(() => validate(env)).toThrow(/content hash mismatch/);
  });

  it("newEnvelope produces distinct transient and node-local ids", () => {
    const a = newEnvelope();
    const b = newEnvelope();
    expect(a.transient.sourcePrimaryId).not.toBe(b.transient.sourcePrimaryId);
    expect(a.nodeLocal.nodeId).not.toBe(b.nodeLocal.nodeId);
  });

  it("optional clusterKeyRef defaults to null and is accepted", () => {
    const env = seal(newEnvelope());
    expect(env.nodeLocal.clusterKeyRef).toBeNull();
    env.nodeLocal.clusterKeyRef = "00000000-0000-4000-8000-000000000099";
    env.contentHash = expectedHash(env);
    expect(() => validate(env)).not.toThrow();
  });
});
