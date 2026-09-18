import { act, createElement } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import { EnrollmentTrustPanel } from "./EnrollmentTrustPanel";
import {
  byRole,
  byTag,
  deferred,
  jsonResponse,
  mount,
  queueFetch,
  settle,
  topology,
  type TestElement,
  TestEvent,
  unmount,
} from "./testDom";

const fingerprint = "0123456789abcdef".repeat(4);
const leafPEM = "-----BEGIN CERTIFICATE-----\nleaf\n-----END CERTIFICATE-----";
const caPEM = "-----BEGIN CERTIFICATE-----\nca\n-----END CERTIFICATE-----";

function panel(identity: ReturnType<typeof topology>) {
  return createElement(
    MemoryRouter,
    null,
    createElement(EnrollmentTrustPanel, { identity }),
  );
}

function panelWithLocation(identity: ReturnType<typeof topology>) {
  function LocationProbe() {
    return createElement("p", null, useLocation().pathname);
  }
  return createElement(
    MemoryRouter,
    null,
    createElement(
      "div",
      null,
      createElement(EnrollmentTrustPanel, { identity }),
      createElement(LocationProbe),
    ),
  );
}

function buttonWithText(container: TestElement, text: string) {
  return byTag(container, "BUTTON").find((button) => button.textContent.includes(text));
}

function installClipboard(writeText: (value: string) => Promise<void>) {
  const original = Object.getOwnPropertyDescriptor(navigator, "clipboard");
  Object.defineProperty(navigator, "clipboard", {
    configurable: true,
    value: { writeText },
  });
  return () => {
    if (original) Object.defineProperty(navigator, "clipboard", original);
    else Reflect.deleteProperty(navigator, "clipboard");
  };
}

function installObjectURL() {
  const originalCreate = URL.createObjectURL;
  const originalRevoke = URL.revokeObjectURL;
  const create = vi.fn(() => "blob:enrollment-ca");
  const revoke = vi.fn();
  Object.defineProperty(URL, "createObjectURL", {
    configurable: true,
    value: create,
  });
  Object.defineProperty(URL, "revokeObjectURL", {
    configurable: true,
    value: revoke,
  });
  return {
    create,
    revoke,
    restore() {
      if (originalCreate) Object.defineProperty(URL, "createObjectURL", { configurable: true, value: originalCreate });
      else Reflect.deleteProperty(URL, "createObjectURL");
      if (originalRevoke) Object.defineProperty(URL, "revokeObjectURL", { configurable: true, value: originalRevoke });
      else Reflect.deleteProperty(URL, "revokeObjectURL");
    },
  };
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("EnrollmentTrustPanel", () => {
  it("shows an accessible loading state and does not render for ineligible topology", async () => {
    queueFetch(new Promise<Response>(() => {}));
    const container = await mount(panel(topology("primary", true)));
    expect(byRole(container, "status")[0]!.textContent).toContain(
      "Loading enrollment trust",
    );

    const fetchMock = vi.fn();
    globalThis.fetch = fetchMock as unknown as typeof fetch;
    const nodeContainer = await mount(panel(topology("node", false)));
    expect(nodeContainer.textContent).toBe("");
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("explains unconfigured trust and asks the Owner to configure SANs", async () => {
    queueFetch(jsonResponse({ status: "unconfigured", configured: false, ready: false }));
    const container = await mount(panel(topology("standalone-primary", true)));

    expect(container.textContent).toContain("Enrollment trust");
    expect(container.textContent).toContain("Configure enrollment SANs above");
    expect(byTag(container, "BUTTON")).toHaveLength(0);
  });

  it("shows not-ready material guidance with a retry action", async () => {
    const fetchMock = queueFetch(
      jsonResponse({ status: "not-ready", configured: true, ready: false }),
      jsonResponse({ status: "ready", configured: true, ready: true, caDerSha256: fingerprint, expiresAt: "2030-01-01T00:00:00Z" }),
    );
    const container = await mount(panel(topology("primary", true)));

    expect(container.textContent).toContain("Enrollment trust is not ready");
    expect(container.textContent).toContain("PRIMARY enrollment runtime");
    const retry = buttonWithText(container, "Retry trust load");
    expect(retry).toBeDefined();
    await act(async () => {
      retry!.dispatchEvent(new TestEvent("click", { bubbles: true }));
      await new Promise((resolve) => setTimeout(resolve, 0));
    });

    expect(container.textContent).toContain("SHA-256 fingerprint");
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("shows the full grouped lowercase fingerprint, expiry, and exact copy value without PEM", async () => {
    const fetchMock = queueFetch(
      jsonResponse({
        status: "ready",
        configured: true,
        ready: true,
        caDerSha256: fingerprint,
        expiresAt: "2030-01-01T00:00:00Z",
        certificatePem: leafPEM,
        caCertificatePem: caPEM,
      }),
    );
    const container = await mount(panel(topology("primary-with-nodes", true)));
    const writeText = vi.fn().mockResolvedValue(undefined);
    const restoreClipboard = installClipboard(writeText);

    expect(container.textContent).toContain("SHA-256 fingerprint");
    expect(container.textContent).toContain("0123 4567 89ab cdef");
    expect(container.textContent).toContain("January 1, 2030");
    expect(container.textContent).not.toContain(leafPEM);
    expect(container.textContent).not.toContain(caPEM);
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/settings/enrollment-trust",
      expect.objectContaining({ cache: "no-store" }),
    );

    const copy = buttonWithText(container, "Copy fingerprint");
    expect(copy).toBeDefined();
    await act(async () => {
      copy!.dispatchEvent(new TestEvent("click", { bubbles: true }));
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
    expect(writeText).toHaveBeenCalledWith(fingerprint);
    expect(byRole(container, "status").some((status) => status.textContent.includes("copied"))).toBe(true);
    restoreClipboard();
  });

  it("prevents duplicate downloads, uses the backend filename, and revokes the Blob URL", async () => {
    const download = deferred<Response>();
    const fetchMock = queueFetch(
      jsonResponse({ status: "ready", configured: true, ready: true, caDerSha256: fingerprint, expiresAt: "2030-01-01T00:00:00Z" }),
      download.promise,
    );
    const urls = installObjectURL();
    const container = await mount(panel(topology("primary", true)));
    const button = buttonWithText(container, "Download CA")!;

    await act(async () => {
      button.dispatchEvent(new TestEvent("click", { bubbles: true }));
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
    button.dispatchEvent(new TestEvent("click", { bubbles: true }));
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(button.disabled).toBe(true);

    await act(async () => {
      download.resolve(new Response(new Blob([caPEM], { type: "application/x-pem-file" }), {
        status: 200,
        headers: { "content-disposition": 'attachment; filename="backend-ca.pem"' },
      }));
      await new Promise((resolve) => setTimeout(resolve, 0));
    });

    expect(urls.create).toHaveBeenCalledTimes(1);
    expect(urls.revoke).toHaveBeenCalledWith("blob:enrollment-ca");
    expect(container.textContent).toContain("Downloaded proxycore-enrollment-ca.pem");
    urls.restore();
  });

  it("reports clipboard rejection as an alert, restores focus, and keeps the fallback node out of the DOM", async () => {
    queueFetch(
      jsonResponse({ status: "ready", configured: true, ready: true, caDerSha256: fingerprint, expiresAt: "2030-01-01T00:00:00Z" }),
    );
    const restoreClipboard = installClipboard(vi.fn().mockRejectedValue(new Error("clipboard denied")));
    const container = await mount(panel(topology("primary", true)));
    const copy = buttonWithText(container, "Copy fingerprint")!;
    copy.focus();

    await act(async () => {
      copy.dispatchEvent(new TestEvent("click", { bubbles: true }));
      await new Promise((resolve) => setTimeout(resolve, 0));
    });

    expect(byRole(container, "alert").some((alert) => alert.textContent.includes("Could not copy fingerprint"))).toBe(true);
    expect(byRole(container, "status")).toHaveLength(0);
    expect(document.activeElement).toBe(copy);
    expect(byTag(container, "TEXTAREA")).toHaveLength(0);
    restoreClipboard();
  });

  it("maps server trust errors to fixed product copy", async () => {
    const secret = "backend certificate path leaked";
    queueFetch(jsonResponse({ error: secret }, 503));
    const unavailable = await mount(panel(topology("primary", true)));
    expect(unavailable.textContent).toContain("Enrollment trust is temporarily unavailable. Retry trust load.");
    expect(unavailable.textContent).not.toContain(secret);

    const fetchMock = queueFetch(
      jsonResponse({ status: "ready", configured: true, ready: true, caDerSha256: fingerprint, expiresAt: "2030-01-01T00:00:00Z" }),
      jsonResponse({ error: secret }, 503),
    );
    const urls = installObjectURL();
    const download = await mount(panel(topology("primary", true)));
    await act(async () => {
      buttonWithText(download, "Download CA")!.dispatchEvent(new TestEvent("click", { bubbles: true }));
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
    expect(byRole(download, "alert").some((alert) => alert.textContent.includes("Enrollment CA is temporarily unavailable. Try again."))).toBe(true);
    expect(download.textContent).not.toContain(secret);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    urls.restore();
  });

  it("redirects 401 to login and hides Owner controls on backend 403", async () => {
    queueFetch(jsonResponse({ error: "Authentication required" }, 401));
    const unauthenticated = await mount(panelWithLocation(topology("primary", true)));
    expect(unauthenticated.textContent).toContain("/login");

    queueFetch(jsonResponse({ error: "Permission denied" }, 403));
    const forbidden = await mount(panel(topology("primary", true)));
    expect(forbidden.textContent).toContain("Owner access is required to view enrollment trust");
    expect(byTag(forbidden, "BUTTON")).toHaveLength(0);
  });

  it("ignores a trust response that arrives after unmount", async () => {
    const pending = deferred<Response>();
    queueFetch(pending.promise);
    const container = await mount(panel(topology("primary", true)));
    await unmount(container);

    await act(async () => {
      pending.resolve(jsonResponse({ status: "ready", configured: true, ready: true, caDerSha256: fingerprint }));
      await settle();
    });
    expect(container.textContent).toBe("");
  });
});
