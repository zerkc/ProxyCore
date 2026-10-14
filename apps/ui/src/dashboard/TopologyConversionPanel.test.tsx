import { act, createElement } from "react";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import { TopologyConversionPanel } from "./TopologyConversionPanel";
import {
  buttonWithText,
  byRole,
  jsonResponse,
  mount,
  queueFetch,
  settle,
  TestEvent,
  topology,
  type TestElement,
} from "./testDom";

type Role = Parameters<typeof topology>[0];

function panel(role: Role, onConverted: () => Promise<void> | void = () => undefined, generation = 4) {
  return createElement(
    MemoryRouter,
    null,
    createElement(TopologyConversionPanel, {
      identity: { ...topology(role, role !== "node"), leadershipGeneration: generation },
      onConverted,
    }),
  );
}

function byTestId(container: TestElement, testId: string) {
  return container.findAll((element) => element.getAttribute("data-testid") === testId)[0];
}

async function click(element: TestElement | undefined) {
  await act(async () => {
    element!.dispatchEvent(new TestEvent("click", { bubbles: true }));
    await settle();
  });
}

function requestBody(fetchMock: ReturnType<typeof queueFetch>, index = 0) {
  const [, init] = fetchMock.mock.calls[index] as [string, RequestInit];
  return JSON.parse(String(init.body));
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("TopologyConversionPanel visibility", () => {
  it("offers only promotion on a NODE", async () => {
    const container = await mount(panel("node"));
    expect(byTestId(container, "topology-promote-open")).toBeDefined();
    expect(byTestId(container, "topology-demote-open")).toBeUndefined();
  });

  it.each(["primary", "primary-with-nodes"] as const)("offers only demotion on %s", async (role) => {
    const container = await mount(panel(role));
    expect(byTestId(container, "topology-demote-open")).toBeDefined();
    expect(byTestId(container, "topology-promote-open")).toBeUndefined();
  });

  it("renders nothing for standalone, stale and unloaded identities", async () => {
    for (const role of ["standalone-primary", "stale-primary"] as const) {
      const container = await mount(panel(role));
      expect(byTestId(container, "topology-promote-open")).toBeUndefined();
      expect(byTestId(container, "topology-demote-open")).toBeUndefined();
    }
    const empty = await mount(
      createElement(MemoryRouter, null, createElement(TopologyConversionPanel, { onConverted: () => undefined })),
    );
    expect(byTestId(empty, "topology-promote-open")).toBeUndefined();
  });
});

describe("TopologyConversionPanel promotion", () => {
  it("requires confirmation and shows the split-brain and re-enroll warnings before any request", async () => {
    const fetchMock = queueFetch();
    const container = await mount(panel("node"));

    await click(byTestId(container, "topology-promote-open"));

    expect(fetchMock).not.toHaveBeenCalled();
    expect(container.textContent).toContain("split-brain");
    expect(container.textContent).toContain("re-enrolled manually");
    expect(byTestId(container, "topology-confirm")).toBeDefined();

    await click(byTestId(container, "topology-cancel"));
    expect(byTestId(container, "topology-confirm")).toBeUndefined();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("posts confirm with the current generation and refreshes identity on success", async () => {
    const fetchMock = queueFetch(jsonResponse({ ok: true, nodesMustReenroll: true }));
    const onConverted = vi.fn().mockResolvedValue(undefined);
    const container = await mount(panel("node", onConverted, 7));

    await click(byTestId(container, "topology-promote-open"));
    await click(byTestId(container, "topology-confirm"));

    expect(fetchMock.mock.calls[0]![0]).toBe("/api/topology/promote");
    expect((fetchMock.mock.calls[0]![1] as RequestInit).method).toBe("POST");
    expect(requestBody(fetchMock)).toEqual({ confirm: true, expectedGeneration: 7 });
    expect(onConverted).toHaveBeenCalledTimes(1);
  });

  it("maps a generation mismatch to a refresh-and-retry message", async () => {
    queueFetch(jsonResponse({ ok: false, code: "GENERATION_MISMATCH", error: "x" }, 409));
    const onConverted = vi.fn();
    const container = await mount(panel("node", onConverted));

    await click(byTestId(container, "topology-promote-open"));
    await click(byTestId(container, "topology-confirm"));

    expect(byRole(container, "alert")[0]!.textContent).toContain("leadership generation changed");
    expect(onConverted).not.toHaveBeenCalled();
  });

  it.each([
    [403, "Owner access is required"],
    [503, "temporarily unavailable"],
    [409, "not allowed from the current role"],
  ])("maps status %s to a clear message", async (status, text) => {
    queueFetch(jsonResponse({ ok: false, code: status === 409 ? "INVALID_TRANSITION" : "X", error: "x" }, status));
    const container = await mount(panel("node"));

    await click(byTestId(container, "topology-promote-open"));
    await click(byTestId(container, "topology-confirm"));

    expect(byRole(container, "alert")[0]!.textContent).toContain(text);
  });
});

describe("TopologyConversionPanel demotion", () => {
  it("states credential loss and read-only NODE before any request", async () => {
    const fetchMock = queueFetch();
    const container = await mount(panel("primary-with-nodes"));

    await click(byTestId(container, "topology-demote-open"));

    expect(fetchMock).not.toHaveBeenCalled();
    expect(container.textContent).toContain("lose their credentials");
    expect(container.textContent).toContain("read-only NODE");
    expect(buttonWithText(container, "Confirm conversion to NODE")).toBeDefined();
  });

  it("posts to demote with the generation and refreshes identity", async () => {
    const fetchMock = queueFetch(jsonResponse({ ok: true, nextStep: "enroll" }));
    const onConverted = vi.fn().mockResolvedValue(undefined);
    const container = await mount(panel("primary", onConverted, 3));

    await click(byTestId(container, "topology-demote-open"));
    await click(byTestId(container, "topology-confirm"));

    expect(fetchMock.mock.calls[0]![0]).toBe("/api/topology/demote");
    expect(requestBody(fetchMock)).toEqual({ confirm: true, expectedGeneration: 3 });
    expect(onConverted).toHaveBeenCalledTimes(1);
  });

  it("shows an error when the network fails", async () => {
    globalThis.fetch = vi.fn().mockRejectedValue(new Error("offline")) as unknown as typeof fetch;
    const container = await mount(panel("primary"));

    await click(byTestId(container, "topology-demote-open"));
    await click(byTestId(container, "topology-confirm"));

    expect(byRole(container, "alert")[0]!.textContent).toContain("could not be reached");
  });
});
