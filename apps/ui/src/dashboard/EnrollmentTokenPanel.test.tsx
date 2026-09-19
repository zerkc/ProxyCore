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
    queueFetch(jsonResponse([]));
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
    expect(fetchMock).toHaveBeenCalledTimes(2);

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
    queueFetch(jsonResponse(list));
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
    expect(fetchMock.mock.calls[2]![0]).toContain("/revoke?confirm=replace-and-revoke");
    expect(container.textContent).toContain("Token revoked");
  });

  it("removes the temporary clipboard fallback node and keeps copy feedback generic", async () => {
    queueFetch(
      jsonResponse([]),
      jsonResponse({ id: "token-id", selector: "selector", token: plaintext, expiresAt: "2030-01-02T00:00:00Z" }, 201),
    );
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
});
