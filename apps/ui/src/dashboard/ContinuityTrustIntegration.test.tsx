import { createElement } from "react";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it, vi } from "vitest";
import { ContinuityView } from "./ContinuityView";
import {
  byTag,
  deferred,
  jsonResponse,
  mount,
  rerender,
  settle,
  topology,
  type TestElement,
} from "./testDom";
import type { TopologyIdentity } from "./types";

function view(identity: TopologyIdentity) {
  return createElement(
    MemoryRouter,
    null,
    createElement(ContinuityView, { identity }),
  );
}

function queueByEndpoint(
  hostnames: Array<Response | Promise<Response>>,
  trust: Array<Response | Promise<Response>>,
) {
  let hostnameIndex = 0;
  let trustIndex = 0;
  const fetchMock = vi.fn((input: RequestInfo | URL, _init?: RequestInit) => {
    const path = String(input);
    return path.includes("/enrollment-trust")
      ? trust[trustIndex++]
      : hostnames[hostnameIndex++];
  });
  globalThis.fetch = fetchMock as unknown as typeof fetch;
  return fetchMock;
}

function fingerprint(container: TestElement) {
  return container.findAll((element) => Boolean(element.getAttribute("data-fingerprint")))[0];
}

describe("ContinuityView trust integration", () => {
  it("reloads both requests after a same-role generation transition and ignores old responses", async () => {
    const firstHostname = deferred<Response>();
    const firstTrust = deferred<Response>();
    const fetchMock = queueByEndpoint(
      [firstHostname.promise, jsonResponse({ configured: true, hostnames: ["new.example"] })],
      [
        firstTrust.promise,
        jsonResponse({
          status: "ready",
          configured: true,
          ready: true,
          caDerSha256: "b".repeat(64),
          expiresAt: "2030-01-01T00:00:00Z",
        }),
      ],
    );
    const initial = topology("primary", true);
    const container = await mount(view(initial));
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(fetchMock.mock.calls[0]![1]).toHaveProperty("signal");

    const next = {
      ...initial,
      leadershipGeneration: 2,
      latestKnownGeneration: 2,
    };
    await rerender(container, view(next));
    await settle();

    expect(fetchMock).toHaveBeenCalledTimes(4);
    expect(byTag(container, "TEXTAREA")[0]!.value).toBe("new.example");
    expect(fingerprint(container)?.getAttribute("data-fingerprint")).toBe("b".repeat(64));

    await firstHostname.resolve(jsonResponse({ configured: true, hostnames: ["old.example"] }));
    await firstTrust.resolve(jsonResponse({
      status: "ready",
      configured: true,
      ready: true,
      caDerSha256: "a".repeat(64),
      expiresAt: "2029-01-01T00:00:00Z",
    }));
    await settle();

    expect(byTag(container, "TEXTAREA")[0]!.value).toBe("new.example");
    expect(container.textContent).not.toContain("old.example");
    expect(fingerprint(container)?.getAttribute("data-fingerprint")).toBe("b".repeat(64));
  });

  it("keeps hostname editing available while mapping trust service errors to fixed copy", async () => {
    const secret = "untrusted backend detail";
    const fetchMock = queueByEndpoint(
      [jsonResponse({ configured: true, hostnames: ["primary.example"] })],
      [jsonResponse({ error: secret }, 503)],
    );
    const container = await mount(view(topology("standalone-primary", true)));

    expect(byTag(container, "TEXTAREA")[0]!.disabled).toBe(false);
    expect(container.textContent).toContain("Enrollment trust is temporarily unavailable. Retry trust load.");
    expect(container.textContent).not.toContain(secret);
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });
});
