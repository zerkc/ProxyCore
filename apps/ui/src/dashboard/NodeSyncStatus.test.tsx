import { act, createElement } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { NodeSyncStatus } from "./NodeSyncStatus";
import { byRole, jsonResponse, mount, queueFetch, settle, TestEvent, type TestElement } from "./testDom";

const status = {
  role: "node",
  bound: true,
  appliedHash: "a1b2c3d4e5f60718293a4b5c6d7e8f90",
  primaryHash: "a1b2c3d4e5f60718293a4b5c6d7e8f90",
  outdated: false,
  lastSyncAt: "2026-10-14T10:00:00Z",
  consecutiveFailures: 0,
  acknowledged: true,
};

function byTestId(container: TestElement, id: string) {
  return container.findAll((element) => element.getAttribute("data-testid") === id)[0];
}

async function click(element: TestElement | undefined) {
  await act(async () => {
    element!.dispatchEvent(new TestEvent("click", { bubbles: true }));
    await settle();
  });
}

afterEach(() => vi.restoreAllMocks());

describe("NodeSyncStatus", () => {
  it("shows In sync with short hashes and the last sync time on a node", async () => {
    queueFetch(jsonResponse(status));
    const container = await mount(createElement(NodeSyncStatus, { role: "node" }));

    expect(byTestId(container, "node-sync-badge")!.textContent).toBe("In sync");
    expect(container.textContent).toContain("a1b2c3d4e5f6");
    expect(container.textContent).not.toContain("a1b2c3d4e5f60718");
    expect(container.textContent).toContain("2026-10-14T10:00:00.000Z");
  });

  it("shows Outdated and the last error", async () => {
    queueFetch(jsonResponse({ ...status, outdated: true, primaryHash: "ffff00001111222233334444", lastError: "primary unreachable" }));
    const container = await mount(createElement(NodeSyncStatus, { role: "node" }));

    expect(byTestId(container, "node-sync-badge")!.textContent).toBe("Outdated");
    expect(byTestId(container, "node-sync-last-error")!.textContent).toContain("primary unreachable");
  });

  it("Sync now posts to retry and refreshes the status", async () => {
    const fetchMock = queueFetch(
      jsonResponse({ ...status, outdated: true }),
      jsonResponse({ ok: true }),
      jsonResponse(status),
    );
    const container = await mount(createElement(NodeSyncStatus, { role: "node" }));

    await click(byTestId(container, "node-sync-now"));

    expect(fetchMock.mock.calls[1]![0]).toBe("/api/topology/sync/retry");
    expect((fetchMock.mock.calls[1]![1] as RequestInit).method).toBe("POST");
    expect(byTestId(container, "node-sync-badge")!.textContent).toBe("In sync");
  });

  it.each([
    [409, "not available right now"],
    [502, "sync attempt failed"],
  ])("maps retry status %s to a message", async (code, text) => {
    queueFetch(jsonResponse(status), jsonResponse({ error: "x" }, code), jsonResponse(status));
    const container = await mount(createElement(NodeSyncStatus, { role: "node" }));

    await click(byTestId(container, "node-sync-now"));

    expect(byRole(container, "alert")[0]!.textContent).toContain(text);
  });

  it("shows the published hash on a primary and no sync button", async () => {
    queueFetch(jsonResponse({ ...status, role: "primary", appliedHash: undefined, primaryHash: "0123456789abcdef0123" }));
    const container = await mount(createElement(NodeSyncStatus, { role: "primary" }));

    expect(byTestId(container, "primary-published-hash")!.textContent).toBe("0123456789ab");
    expect(byTestId(container, "node-sync-now")).toBeUndefined();
  });

  it("renders nothing and makes no request for a standalone installation", async () => {
    const fetchMock = queueFetch();
    const container = await mount(createElement(NodeSyncStatus, { role: "standalone-primary" }));

    expect(byTestId(container, "node-sync-status")).toBeUndefined();
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
