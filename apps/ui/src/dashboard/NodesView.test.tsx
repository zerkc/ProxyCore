import { act, createElement } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { describeLiveness, NODE_OFFLINE_THRESHOLD_MS, NodesView } from "./NodesView";
import { jsonResponse, mount, queueFetch, settle, TestEvent, type TestElement } from "./testDom";
import type { TopologyNode } from "./types";

const SERVER_TIME = "2026-10-14T12:00:00Z";

function node(overrides: Partial<TopologyNode>): TopologyNode {
  return {
    nodeId: "11111111-2222-3333-4444-555555555555",
    displayName: "edge-a",
    status: "linked",
    ingressIPv4: "203.0.113.7",
    ingressIPv6: "",
    lastSeenAt: "2026-10-14T11:59:30Z",
    enrolledAt: "2026-10-01T00:00:00Z",
    unlinkedAt: null,
    ...overrides,
  };
}

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

describe("describeLiveness", () => {
  it("ages lastSeenAt against the server time and flags stale linked nodes", () => {
    expect(describeLiveness(node({ lastSeenAt: "2026-10-14T11:59:30Z" }), SERVER_TIME)).toEqual({ lastSeen: "30s ago", offline: false });
    const stale = new Date(Date.parse(SERVER_TIME) - NODE_OFFLINE_THRESHOLD_MS - 1000).toISOString();
    expect(describeLiveness(node({ lastSeenAt: stale }), SERVER_TIME).offline).toBe(true);
    expect(describeLiveness(node({ lastSeenAt: null }), SERVER_TIME)).toEqual({ lastSeen: "never", offline: true });
  });

  it("never marks unlinked or revoked nodes offline", () => {
    expect(describeLiveness(node({ status: "unlinked", lastSeenAt: null }), SERVER_TIME).offline).toBe(false);
    expect(describeLiveness(node({ status: "revoked", lastSeenAt: "2020-01-01T00:00:00Z" }), SERVER_TIME).offline).toBe(false);
  });
});

describe("NodesView", () => {
  it("renders nodes with gateway IPs, relative last seen and an offline badge", async () => {
    queueFetch(jsonResponse({
      serverTime: SERVER_TIME,
      nodes: [
        node({}),
        node({ nodeId: "aaaaaaaa-0000-0000-0000-000000000000", displayName: "", ingressIPv4: "", ingressIPv6: "2001:db8::1", lastSeenAt: "2026-10-14T11:00:00Z" }),
      ],
    }));
    const container = await mount(createElement(NodesView, { role: "primary" }));

    const rows = container.findAll((el) => (el.getAttribute("data-testid") ?? "").startsWith("node-row-"));
    expect(rows).toHaveLength(2);
    expect(rows[0]!.textContent).toContain("edge-a");
    expect(rows[0]!.textContent).toContain("IPv4 203.0.113.7");
    expect(rows[0]!.textContent).toContain("IPv6 —");
    expect(rows[0]!.textContent).toContain("last seen 30s ago");
    expect(rows[0]!.textContent).not.toContain("offline");
    expect(rows[1]!.textContent).toContain("aaaaaaaa");
    expect(rows[1]!.textContent).toContain("IPv6 2001:db8::1");
    expect(rows[1]!.textContent).toContain("last seen 1h ago");
    expect(rows[1]!.textContent).toContain("offline");
  });

  it("does not call the API on a non-primary role", async () => {
    const fetchMock = queueFetch();
    const container = await mount(createElement(NodesView, { role: "node" }));
    expect(byTestId(container, "nodes-unavailable")).toBeDefined();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("shows the blocked state without an API call when the role is unknown", async () => {
    const fetchMock = queueFetch();
    const container = await mount(createElement(NodesView, {}));
    expect(byTestId(container, "nodes-unavailable")).toBeDefined();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("explains how to link a node when the list is empty", async () => {
    queueFetch(jsonResponse({ serverTime: SERVER_TIME, nodes: [] }));
    const container = await mount(createElement(NodesView, { role: "primary-with-nodes" }));
    expect(byTestId(container, "nodes-empty")!.textContent).toContain("enrollment token");
  });

  it("requires confirmation, posts the unlink and refreshes the list", async () => {
    const fetchMock = queueFetch(
      jsonResponse({ serverTime: SERVER_TIME, nodes: [node({})] }),
      jsonResponse({ nodeId: node({}).nodeId, unlinkedAt: SERVER_TIME }),
      jsonResponse({ serverTime: SERVER_TIME, nodes: [node({ status: "unlinked", unlinkedAt: SERVER_TIME })] }),
    );
    const container = await mount(createElement(NodesView, { role: "primary" }));

    await click(byTestId(container, "node-unlink-open"));
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(byTestId(container, "node-unlink-confirm")!.textContent).toContain("keeps serving its last configuration");

    await click(byTestId(container, "node-unlink-confirm-button"));
    expect(fetchMock.mock.calls[1]![0]).toBe(`/api/topology/nodes/${node({}).nodeId}/unlink`);
    expect((fetchMock.mock.calls[1]![1] as RequestInit).method).toBe("POST");
    expect(byTestId(container, "node-status")!.textContent).toBe("unlinked");
    expect(byTestId(container, "node-unlink-open")).toBeUndefined();
  });

  it.each([
    [403, {}, "Owner access"],
    [404, { error: "node not found" }, "no longer exists"],
    [409, { code: "TOPOLOGY_NOT_PRIMARY" }, "only be managed from a PRIMARY"],
    [503, {}, "temporarily unavailable"],
  ])("maps unlink status %s to a message", async (code, body, text) => {
    queueFetch(
      jsonResponse({ serverTime: SERVER_TIME, nodes: [node({})] }),
      jsonResponse(body, code),
      jsonResponse({ serverTime: SERVER_TIME, nodes: [node({})] }),
    );
    const container = await mount(createElement(NodesView, { role: "primary" }));
    await click(byTestId(container, "node-unlink-open"));
    await click(byTestId(container, "node-unlink-confirm-button"));
    expect(byTestId(container, "nodes-action-error")!.textContent).toContain(text);
  });

  it("shows a load error instead of the empty state", async () => {
    queueFetch(jsonResponse({ code: "TOPOLOGY_NOT_PRIMARY" }, 409));
    const container = await mount(createElement(NodesView, { role: "primary" }));
    expect(byTestId(container, "nodes-load-error")).toBeDefined();
    expect(byTestId(container, "nodes-empty")).toBeUndefined();
  });
});
