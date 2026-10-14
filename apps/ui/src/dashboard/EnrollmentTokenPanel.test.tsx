import { act, createElement } from "react";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import { EnrollmentTokenPanel } from "./EnrollmentTokenPanel";
import {
  byRole,
  byTag,
  buttonWithText,
  jsonResponse,
  mount,
  queueFetch,
  settle,
  TestEvent,
  type TestElement,
} from "./testDom";

const plaintext = "pcenr1_selector_secret-must-not-appear";

function panel() {
  return createElement(
    MemoryRouter,
    null,
    createElement(EnrollmentTokenPanel),
  );
}

function input(container: TestElement, testId: string) {
  return container.findAll(
    (element) => element.getAttribute("data-testid") === testId,
  )[0]!;
}

function setValue(element: TestElement, value: string) {
  act(() => {
    element.value = value;
    element.dispatchEvent(new TestEvent("input", { bubbles: true }));
    element.dispatchEvent(new TestEvent("change", { bubbles: true }));
  });
}

function defaultsResponse(overrides: Record<string, unknown> = {}) {
  return jsonResponse({
    primaryUrl: "https://203.0.113.10:3443/",
    gatewayIPv4: "203.0.113.10",
    port: 3443,
    trustReady: true,
    hostCovered: true,
    ...overrides,
  });
}

async function click(container: TestElement, testId: string) {
  await act(async () => {
    input(container, testId).dispatchEvent(new TestEvent("click", { bubbles: true }));
    await settle();
  });
}

const created = { id: "token-id", selector: "selector", token: plaintext, expiresAt: "2030-01-02T00:00:00Z" };

afterEach(() => {
  vi.restoreAllMocks();
});

describe("EnrollmentTokenPanel", () => {
  it("renders an accessible loading state before the token list arrives", async () => {
    queueFetch(new Promise<Response>(() => {}));
    const container = await mount(panel());

    expect(byRole(container, "status")[0]!.textContent).toContain(
      "Loading enrollment tokens",
    );
  });

  it("renders the empty state and create action", async () => {
    queueFetch(jsonResponse([]), defaultsResponse());
    const container = await mount(panel());

    expect(container.textContent).toContain("No enrollment tokens");
    expect(buttonWithText(container, "Create enrollment token")).toBeDefined();
    expect(
      buttonWithText(container, "Create enrollment token")!.getAttribute("data-testid"),
    ).toBe("create-enrollment-token");
  });

  it("shows the plaintext once, copies generically, downloads, and wipes it after acknowledgement", async () => {
    const fetchMock = queueFetch(
      jsonResponse([]),
      defaultsResponse(),
      jsonResponse({
        id: "token-id",
        selector: "selector",
        token: plaintext,
        expiresAt: "2030-01-02T00:00:00Z",
      }, 201),
    );
    const container = await mount(panel());
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: { writeText },
    });

    await act(async () => {
      buttonWithText(container, "Create enrollment token")!.dispatchEvent(
        new TestEvent("click", { bubbles: true }),
      );
      await settle();
    });

    expect(input(container, "enrollment-token-value").value).toBe(plaintext);
    expect(container.textContent).toContain("This token will not be shown again");
    const copy = buttonWithText(container, "Copy token")!;
    await act(async () => {
      copy.dispatchEvent(new TestEvent("click", { bubbles: true }));
      await settle();
    });
    expect(writeText).toHaveBeenCalledWith(plaintext);
    expect(container.textContent).toContain("Token copied");
    expect(container.textContent).not.toContain("server detail");
    expect(fetchMock).toHaveBeenCalledTimes(3);

    await act(async () => {
      buttonWithText(container, "I have saved this token")!.dispatchEvent(
        new TestEvent("click", { bubbles: true }),
      );
      await settle();
    });
    expect(container.textContent).not.toContain(plaintext);
    expect(byTag(container, "TEXTAREA")).toHaveLength(0);
    expect(container.textContent).toContain("Token saved");
  });

  it("renders lifecycle summaries without any plaintext column", async () => {
    const list = [
      {
        id: "token-id",
        selector: "selector",
        createdAt: "2030-01-01T00:00:00Z",
        expiresAt: "2030-01-02T00:00:00Z",
        consumedAt: null,
        revokedAt: "2030-01-01T12:00:00Z",
      },
    ];
    queueFetch(jsonResponse(list), defaultsResponse());
    const container = await mount(
      createElement(MemoryRouter, null, createElement(EnrollmentTokenPanel)),
    );

    expect(container.textContent).toContain("selector");
    expect(container.textContent).toContain("2030-01-01");
    expect(container.textContent).not.toContain(plaintext);
    expect(buttonWithText(container, "Revoke")).toBeDefined();
  });

  it("requires explicit revoke confirmation and reports a server rejection without leaking details", async () => {
    const secret = "revocation backend detail";
    const fetchMock = queueFetch(
      jsonResponse([
        {
          id: "token-id",
          selector: "selector",
          createdAt: "2030-01-01T00:00:00Z",
          expiresAt: "2030-01-02T00:00:00Z",
        },
      ]),
      defaultsResponse(),
      jsonResponse({ error: secret }, 409),
      new Response(null, { status: 204 }),
      jsonResponse([]),
    );
    const container = await mount(panel());
    await act(async () => {
      buttonWithText(container, "Revoke")!.dispatchEvent(
        new TestEvent("click", { bubbles: true }),
      );
      await settle();
    });
    await act(async () => {
      input(container, "revoke-confirmation-token-id").dispatchEvent(
        new TestEvent("submit", { bubbles: true, cancelable: true }),
      );
      await settle();
    });
    expect(container.textContent).toContain("Type replace-and-revoke");
    expect(container.textContent).not.toContain(secret);

    setValue(input(container, "revoke-confirmation-token-id"), "replace-and-revoke");
    await settle();
    await act(async () => {
      container.findAll(
        (candidate) => candidate.getAttribute("data-testid") === "revoke-enrollment-form-token-id",
      )[0]!.dispatchEvent(new TestEvent("submit", { bubbles: true, cancelable: true }));
      await settle();
    });
    expect(fetchMock.mock.calls[3]![0]).toContain("/revoke?confirm=replace-and-revoke");
    expect(container.textContent).toContain("Token revoked");
  });

  it("shows delete only for revoked or expired unconsumed tokens and confirms before deleting", async () => {
    const base = { selector: "s", createdAt: "2020-01-01T00:00:00Z" };
    const list = [
      { ...base, id: "revoked", expiresAt: "2099-01-01T00:00:00Z", revokedAt: "2020-01-01T01:00:00Z" },
      { ...base, id: "expired", expiresAt: "2020-01-01T00:15:00Z" },
      { ...base, id: "active", expiresAt: "2099-01-01T00:00:00Z" },
      { ...base, id: "consumed", expiresAt: "2020-01-01T00:15:00Z", consumedAt: "2020-01-01T00:05:00Z" },
    ];
    const fetchMock = queueFetch(
      jsonResponse(list),
      defaultsResponse(),
      new Response(null, { status: 204 }),
      jsonResponse(list.filter((token) => token.id !== "revoked")),
    );
    const container = await mount(panel());
    const has = (id: string) => container.findAll((c) => c.getAttribute("data-testid") === `delete-enrollment-token-${id}`).length > 0;
    expect([has("revoked"), has("expired"), has("active"), has("consumed")]).toEqual([true, true, false, false]);
    expect(container.textContent).toContain("Manage that node in the Nodes view");

    await click(container, "delete-enrollment-token-revoked");
    expect(fetchMock).toHaveBeenCalledTimes(2);
    await click(container, "confirm-delete-revoked");
    expect(fetchMock.mock.calls[2]![0]).toBe("/api/enrollment/tokens/revoked");
    expect(fetchMock.mock.calls[2]![1]).toMatchObject({ method: "DELETE" });
    expect(container.textContent).toContain("Token deleted");
    expect(has("revoked")).toBe(false);
  });

  it.each([
    [409, { code: "TOKEN_ACTIVE", error: "x" }, "still active"],
    [409, { code: "TOKEN_IN_USE", error: "x" }, "Nodes view"],
    [404, { error: "x" }, "no longer exists"],
    [403, { error: "x" }, "Owner access"],
    [503, { error: "x" }, "temporarily unavailable"],
  ])("maps delete failure %s to a clear message", async (status, body, text) => {
    queueFetch(
      jsonResponse([{ id: "t", selector: "s", createdAt: "2020-01-01T00:00:00Z", expiresAt: "2099-01-01T00:00:00Z", revokedAt: "2020-01-01T01:00:00Z" }]),
      defaultsResponse(),
      jsonResponse(body, status),
    );
    const container = await mount(panel());
    await click(container, "delete-enrollment-token-t");
    await click(container, "confirm-delete-t");
    expect(container.textContent).toContain(text);
  });

  it("removes the temporary clipboard fallback node and keeps copy feedback generic", async () => {
    queueFetch(jsonResponse([]), defaultsResponse(), jsonResponse(created, 201));
    const container = await mount(panel());
    await act(async () => {
      buttonWithText(container, "Create enrollment token")!.dispatchEvent(new TestEvent("click", { bubbles: true }));
      await settle();
    });
    const originalClipboard = Object.getOwnPropertyDescriptor(navigator, "clipboard");
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText: vi.fn().mockRejectedValue(new Error("denied")) } });
    const execCommand = vi.spyOn(document, "execCommand").mockReturnValue(true);
    await act(async () => {
      buttonWithText(container, "Copy token")!.dispatchEvent(new TestEvent("click", { bubbles: true }));
      await settle();
    });
    expect(execCommand).toHaveBeenCalledWith("copy");
    expect(byTag(container, "TEXTAREA")).toHaveLength(1);
    expect(container.textContent).not.toContain("clipboard denied");
    if (originalClipboard) Object.defineProperty(navigator, "clipboard", originalClipboard);
  });

  describe("primary URL", () => {
    it("prefills the editable field from the defaults and sends an edited value", async () => {
      const fetchMock = queueFetch(jsonResponse([]), defaultsResponse(), jsonResponse(created, 201));
      const container = await mount(panel());
      expect(input(container, "enrollment-token-primary-url").value).toBe("https://203.0.113.10:3443/");
      expect(container.textContent).toContain("The node will connect to this address");
      expect(container.textContent).toContain("edit it here before generating");
      expect(fetchMock.mock.calls[1]![0]).toBe("/api/enrollment/tokens/defaults");
      setValue(input(container, "enrollment-token-primary-url"), "https://203.0.113.10:4443/");
      await click(container, "create-enrollment-token");
      const call = fetchMock.mock.calls[2]!;
      expect(call[0]).toBe("/api/enrollment/tokens");
      expect(JSON.parse(String(call[1].body))).toEqual({ primaryUrl: "https://203.0.113.10:4443/" });
    });

    it("omits primaryUrl when the field is empty", async () => {
      const fetchMock = queueFetch(jsonResponse([]), defaultsResponse(), jsonResponse(created, 201));
      const container = await mount(panel());
      setValue(input(container, "enrollment-token-primary-url"), "");
      await click(container, "create-enrollment-token");
      expect(JSON.parse(String(fetchMock.mock.calls[2]![1].body))).toEqual({});
    });

    it("does not block generating when the defaults fail to load", async () => {
      const fetchMock = queueFetch(jsonResponse([]), jsonResponse({ error: "boom" }, 500), jsonResponse(created, 201));
      const container = await mount(panel());
      expect(input(container, "enrollment-token-primary-url").value).toBe("");
      expect(container.textContent).not.toContain("not ready");
      await click(container, "create-enrollment-token");
      expect(JSON.parse(String(fetchMock.mock.calls[2]![1].body))).toEqual({});
      expect(input(container, "enrollment-token-value").value).toBe(plaintext);
    });

    it("keeps a long v2 token in the read-only surface and copies it whole", async () => {
      const longToken = `pcenr2_${"A".repeat(2400)}`;
      queueFetch(jsonResponse([]), defaultsResponse(), jsonResponse({ ...created, token: longToken }, 201));
      const container = await mount(panel());
      const writeText = vi.fn().mockResolvedValue(undefined);
      Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
      await click(container, "create-enrollment-token");
      expect(input(container, "enrollment-token-value").value).toBe(longToken);
      await click(container, "copy-enrollment-token");
      expect(writeText).toHaveBeenCalledWith(longToken);
    });

    it("warns, without blocking, when trust is not ready or the host is not covered", async () => {
      queueFetch(jsonResponse([]), defaultsResponse({ trustReady: false, hostCovered: false }));
      const trust = await mount(panel());
      expect(input(trust, "enrollment-token-trust-warning")).toBeDefined();
      expect(input(trust, "create-enrollment-token").getAttribute("disabled")).toBeFalsy();

      queueFetch(jsonResponse([]), defaultsResponse({ hostCovered: false }));
      const host = await mount(panel());
      expect(input(host, "enrollment-token-host-warning")).toBeDefined();
      expect(host.textContent).toContain("enrollment hostnames");
    });

    const errorCases: Array<[string, number, string]> = [
      ["ENROLLMENT_TRUST_NOT_READY", 409, "enrollment trust panel"],
      ["ENROLLMENT_TRUST_UNAVAILABLE", 503, "temporarily unavailable"],
      ["PRIMARY_URL_REQUIRED", 422, "Enter the primary URL"],
      ["PRIMARY_URL_INVALID", 422, "https://host:port"],
      ["PRIMARY_URL_NOT_IN_CERT", 422, "enrollment hostnames"],
    ];
    for (const [code, status, expected] of errorCases) {
      it(`explains ${code}`, async () => {
        queueFetch(jsonResponse([]), defaultsResponse(), jsonResponse({ error: "server detail secret", code }, status));
        const container = await mount(panel());
        await click(container, "create-enrollment-token");
        const alert = byRole(container, "alert").map((node) => node.textContent).join(" ");
        expect(alert).toContain(expected);
        expect(alert).not.toContain("server detail secret");
        expect(container.textContent).not.toContain(plaintext);
      });
    }
  });
});
