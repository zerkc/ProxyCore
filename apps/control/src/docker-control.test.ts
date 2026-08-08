import { describe, expect, it } from "vitest";
import { assertCandidatePath } from "./docker-control";

describe("Docker control boundary", () => {
  it("accepts only candidates below the fixed shared root", () => {
    expect(() =>
      assertCandidatePath(
        "/var/lib/proxycore/candidates/revision-1",
        "/var/lib/proxycore/candidates",
      ),
    ).not.toThrow();
  });

  it("rejects traversal and unrelated filesystem paths", () => {
    expect(() =>
      assertCandidatePath("/etc/passwd", "/var/lib/proxycore/candidates"),
    ).toThrow(/fixed worker root/i);
    expect(() =>
      assertCandidatePath(
        "/var/lib/proxycore/candidates/../secrets",
        "/var/lib/proxycore/candidates",
      ),
    ).toThrow(/fixed worker root/i);
  });
});
