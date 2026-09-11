import { describe, expect, it } from "vitest";
import {
  CLUSTER_KEK_LENGTH,
  generateClusterKEK,
  isClusterKEKEnvelope,
  unwrapWithClusterKEK,
  wrapWithClusterKEK,
} from "./cluster";

describe("cluster KEK envelope", () => {
  it("round-trips a UTF-8 string", () => {
    const kek = generateClusterKEK();
    const env = wrapWithClusterKEK("hello, replicated secret", kek);
    const got = unwrapWithClusterKEK(env, kek).toString("utf8");
    expect(got).toBe("hello, replicated secret");
  });

  it("round-trips a binary buffer", () => {
    const kek = generateClusterKEK();
    const data = Buffer.from([0xde, 0xad, 0xbe, 0xef, 0x00, 0xff]);
    const env = wrapWithClusterKEK(data, kek);
    const got = unwrapWithClusterKEK(env, kek);
    expect(got.equals(data)).toBe(true);
  });

  it("produces distinct envelopes for the same plaintext (random IV)", () => {
    const kek = generateClusterKEK();
    const a = wrapWithClusterKEK("same plaintext", kek);
    const b = wrapWithClusterKEK("same plaintext", kek);
    expect(a).not.toBe(b);
  });

  it("recognises its own envelope prefix and rejects others", () => {
    const kek = generateClusterKEK();
    const env = wrapWithClusterKEK("x", kek);
    expect(isClusterKEKEnvelope(env)).toBe(true);
    expect(isClusterKEKEnvelope("v1:something-else")).toBe(false);
    expect(isClusterKEKEnvelope("not-an-envelope")).toBe(false);
  });

  it("rejects a wrong KEK", () => {
    const kekA = generateClusterKEK();
    const kekB = generateClusterKEK();
    const env = wrapWithClusterKEK("top secret", kekA);
    expect(() => unwrapWithClusterKEK(env, kekB)).toThrow();
  });

  it("rejects tampered ciphertext", () => {
    const kek = generateClusterKEK();
    const env = wrapWithClusterKEK("do not modify", kek);
    const segments = env.split(":");
    const tampered = flipBase64Char(segments[3]);
    const bad = [segments[0], segments[1], segments[2], tampered].join(":");
    expect(() => unwrapWithClusterKEK(bad, kek)).toThrow();
  });

  it("rejects tampered tag", () => {
    const kek = generateClusterKEK();
    const env = wrapWithClusterKEK("do not modify", kek);
    const segments = env.split(":");
    const tampered = flipBase64Char(segments[2]);
    const bad = [segments[0], segments[1], tampered, segments[3]].join(":");
    expect(() => unwrapWithClusterKEK(bad, kek)).toThrow();
  });

  it("rejects a non-envelope input", () => {
    const kek = generateClusterKEK();
    const inputs = [
      "",
      "v1:not-a-kek-envelope",
      "v1.kek",
      "v1.kek:bad:bad:bad",
      "not-an-envelope",
      "v1.kek:::" + "AAAA",
    ];
    for (const value of inputs) {
      try {
        unwrapWithClusterKEK(value, kek);
        throw new Error(`expected unwrap(${JSON.stringify(value)}) to throw`);
      } catch (error) {
        if (error instanceof Error && error.message.startsWith("expected unwrap(")) {
          throw error;
        }
        // otherwise the unwrap correctly threw
      }
    }
  });

  it("rejects an invalid KEK length", () => {
    expect(() => wrapWithClusterKEK("x", Buffer.alloc(16))).toThrow(/32-byte/);
    expect(() => wrapWithClusterKEK("x", Buffer.alloc(0))).toThrow(/32-byte/);
    expect(() => unwrapWithClusterKEK("v1.kek:AA:BB:CC", Buffer.alloc(31))).toThrow(/32-byte/);
  });

  it("generates 32-byte KEKs", () => {
    expect(generateClusterKEK().length).toBe(CLUSTER_KEK_LENGTH);
  });
});

function flipBase64Char(s: string): string {
  if (s === "A") return "B";
  return "A";
}
