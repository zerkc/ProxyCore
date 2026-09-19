import { act, createElement } from "react";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import { NodeContinuityView } from "./NodeContinuityView";
import {
  byRole,
  deferred,
  jsonResponse,
  mount,
  queueFetch,
  settle,
  TestEvent,
  type TestElement,
  unmount,
} from "./testDom";

const plaintext = "pcenr1_selector_secret-must-not-appear";
const credential = "pcnode1_credential-must-not-appear";
const kek = "cluster-kek-must-not-appear";

function view() {
  return createElement(
    MemoryRouter,
    null,
    createElement(NodeContinuityView),
  );
}

function element(container: TestElement, testId: string) {
  return container.findAll(
    (candidate) => candidate.getAttribute("data-testid") === testId,
  )[0]!;
}

function setValue(candidate: TestElement, value: string) {
  act(() => {
    candidate.value = value;
    candidate.dispatchEvent(new TestEvent("input", { bubbles: true }));
    candidate.dispatchEvent(new TestEvent("change", { bubbles: true }));
  });
}

function previewResponse() {
  return jsonResponse({
    draftId: "draft-id",
    envelopePreview: {
      ingress: { ipv4: "192.0.2.20" },
      role: "node",
      generation: 7,
      clusterKeyId: "cluster-key-id",
      contentHash: "content-hash",
      credential,
      kek,
    },
    nodeLocalOverlay: { nodeId: "node-id", role: "node", ingress: { ipv4: "192.0.2.20" } },
    expiresAt: "2030-01-02T00:00:00Z",
    token: plaintext,
  });
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("NodeContinuityView", () => {
  it("renders a recoverable token-missing prompt after reload recovery", async () => {
    queueFetch(jsonResponse({ error: "token detail must not be shown" }, 410));
    const container = await mount(view());

    expect(container.textContent).toContain("Please re-enter your enrollment token");
    expect(element(container, "node-enrollment-token")).toBeDefined();
    expect(byRole(container, "status")).toHaveLength(0);
    expect(container.textContent).not.toContain(plaintext);
  });

  it("submits the token and URL, renders a redacted preview, then commits", async () => {
    const confirmation = deferred<Response>();
    const fetchMock = queueFetch(
      jsonResponse({ error: "token-missing" }, 410),
      previewResponse(),
      confirmation.promise,
    );
    const container = await mount(view());
    setValue(element(container, "node-enrollment-token"), plaintext);
    setValue(element(container, "node-primary-url"), "https://primary.example");
    await settle();

    await act(async () => {
      element(container, "node-enrollment-form").dispatchEvent(
        new TestEvent("submit", { bubbles: true, cancelable: true }),
      );
      await settle();
    });
    expect(container.textContent).toContain("Enrollment preview");
    expect(container.textContent).toContain("192.0.2.20");
    expect(container.textContent).toContain("content-hash");
    expect(container.textContent).not.toContain(plaintext);
    expect(container.textContent).not.toContain(credential);
    expect(container.textContent).not.toContain(kek);
    expect(element(container, "node-confirm")).toBeDefined();

    await act(async () => {
      element(container, "node-confirm").dispatchEvent(
        new TestEvent("click", { bubbles: true }),
      );
      await settle();
    });
    expect(container.textContent).toContain("Applying enrollment");
    expect(container.textContent).not.toContain(credential);
    await act(async () => {
      confirmation.resolve(jsonResponse({
        role: "node",
        generation: 7,
        nodeId: "node-id",
        clusterKeyId: "cluster-key-id",
        archiveId: "archive-id",
        applyJobId: "apply-job-id",
      }));
      await settle();
    });
    expect(container.textContent).toContain("Enrollment committed");
    expect(container.textContent).toContain("apply-job-id");
    expect(container.textContent).toContain("archive-id");
    expect(container.textContent).not.toContain(plaintext);
    expect(container.textContent).not.toContain(credential);
    expect(container.textContent).not.toContain(kek);
    expect(fetchMock.mock.calls[1]![1]).toMatchObject({
      body: JSON.stringify({ token: plaintext, primaryURL: "https://primary.example" }),
    });
  });

  it("returns to preview with generic error on confirm conflict and supports cancel", async () => {
    queueFetch(
      jsonResponse({ error: "token-missing" }, 410),
      previewResponse(),
      jsonResponse({ error: credential }, 409),
    );
    const container = await mount(view());
    setValue(element(container, "node-enrollment-token"), plaintext);
    setValue(element(container, "node-primary-url"), "https://primary.example");
    await settle();
    await act(async () => {
      element(container, "node-enrollment-form").dispatchEvent(new TestEvent("submit", { bubbles: true, cancelable: true }));
      await settle();
    });
    await act(async () => {
      element(container, "node-confirm").dispatchEvent(new TestEvent("click", { bubbles: true }));
      await settle();
    });
    expect(container.textContent).toContain("Enrollment confirmation was rejected");
    expect(container.textContent).not.toContain(credential);
    expect(element(container, "node-cancel")).toBeDefined();
    await act(async () => {
      element(container, "node-cancel").dispatchEvent(new TestEvent("click", { bubbles: true }));
      await settle();
    });
    expect(container.textContent).toContain("Paste your enrollment token");
  });

  it("keeps pending output generic and aborts recovery on unmount", async () => {
    const pending = deferred<Response>();
    const fetchMock = queueFetch(pending.promise);
    const container = await mount(view());
    await unmount(container);
    expect(fetchMock.mock.calls[0]![1]).toMatchObject({ signal: expect.any(AbortSignal) });
    expect((fetchMock.mock.calls[0]![1] as RequestInit).signal?.aborted).toBe(true);
    await act(async () => {
      pending.resolve(jsonResponse({ token: credential }, 200));
      await settle();
    });
    expect(container.textContent).toBe("");
  });

  it("downloads only the safe post-conversion audit projection", async () => {
    queueFetch(
      jsonResponse({ error: "token-missing" }, 410),
      previewResponse(),
      jsonResponse({
        role: "node",
        generation: 7,
        nodeId: "node-id",
        clusterKeyId: "cluster-key-id",
        archiveId: "archive-id",
        applyJobId: "apply-job-id",
      }),
    );
    const container = await mount(view());
    setValue(element(container, "node-enrollment-token"), plaintext);
    setValue(element(container, "node-primary-url"), "https://primary.example");
    await settle();
    await act(async () => {
      element(container, "node-enrollment-form").dispatchEvent(new TestEvent("submit", { bubbles: true, cancelable: true }));
      await settle();
    });
    await act(async () => {
      element(container, "node-confirm").dispatchEvent(new TestEvent("click", { bubbles: true }));
      await settle();
      await settle();
    });
    const createObjectURL = vi.spyOn(URL, "createObjectURL").mockReturnValue("blob:audit");
    const revokeObjectURL = vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => undefined);
    await act(async () => {
      element(container, "node-audit-download").dispatchEvent(new TestEvent("click", { bubbles: true }));
    });
    expect(createObjectURL).toHaveBeenCalledTimes(1);
    const blob = createObjectURL.mock.calls[0]![0] as Blob;
    const downloaded = await blob.text();
    expect(downloaded).toContain("apply-job-id");
    expect(downloaded).not.toContain(plaintext);
    expect(downloaded).not.toContain(credential);
    expect(downloaded).not.toContain(kek);
    expect(revokeObjectURL).toHaveBeenCalledWith("blob:audit");
  });
});
