import { createElement } from "react";
import { describe, expect, it, vi } from "vitest";
import { NodeUnlinkedBanner } from "./NodeUnlinkedBanner";
import { mount } from "./testDom";

describe("NodeUnlinkedBanner", () => {
  it("explains the state and offers re-link and convert actions", async () => {
    const container = await mount(createElement(NodeUnlinkedBanner, { unlinkedAt: "2026-10-14T10:00:00Z" }));
    const text = container.textContent;
    expect(text).toContain("unlinked by its PRIMARY");
    expect(text).toContain("keeps serving its last configuration");
    expect(text).toContain("Re-link to a primary");
    expect(text).toContain("Convert to PRIMARY");
    expect(container.findAll((el) => el.getAttribute("role") === "alert")).toHaveLength(1);
    vi.restoreAllMocks();
  });
});
