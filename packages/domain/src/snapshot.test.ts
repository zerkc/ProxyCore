import { describe, expect, it } from "vitest";
import { checksumSnapshot, createSnapshot } from "./snapshot";

describe("configuration snapshots", () => {
  it("produces the same checksum regardless of object key order", () => {
    const left = createSnapshot({ b: 2, a: { z: true, y: false } });
    const right = createSnapshot({ a: { y: false, z: true }, b: 2 });

    expect(checksumSnapshot(left)).toBe(checksumSnapshot(right));
  });
});
