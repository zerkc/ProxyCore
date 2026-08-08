import { describe, expect, it } from "vitest";
import { checksumSnapshot, createSnapshot } from "./snapshot";

describe("configuration snapshots", () => {
  it("produces the same checksum regardless of object key order", () => {
    const left = createSnapshot({ b: 2, a: { z: true, y: false } });
    const right = createSnapshot({ a: { y: false, z: true }, b: 2 });

    expect(checksumSnapshot(left)).toBe(checksumSnapshot(right));
  });

  it("keeps optional undefined fields valid for JSON snapshots", () => {
    const snapshot = createSnapshot({
      settings: { ingress: { ipv4: undefined, ipv6: undefined } },
      values: [undefined],
    });

    expect(snapshot).toEqual({
      settings: { ingress: {} },
      values: [null],
    });
    expect(checksumSnapshot(snapshot)).toHaveLength(64);
  });
});
