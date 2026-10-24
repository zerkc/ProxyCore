import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { VersionStatus } from "./VersionStatus";
import type { StatusPayload, UpdatePayload } from "./types";
import {
  buttonWithText,
  byRole,
  createElement,
  jsonResponse,
  mount,
  rerender,
  type TestElement,
} from "./testDom";

type RuntimeStatus = Pick<
  StatusPayload,
  "version" | "updating" | "updateTarget" | "updateProgress"
>;

const update: UpdatePayload = {
  status: "update_available",
  currentVersion: "0.6.11",
  updateAvailable: true,
  latest: {
    version: "0.6.12",
    tag: "v0.6.12",
    url: "https://example.test/release",
    publishedAt: null,
  },
} as UpdatePayload;

const body = () => document.body as unknown as TestElement;

function render(props: {
  status?: RuntimeStatus;
  onApplyingChange?: (applying: boolean) => void;
  onRetry?: () => void;
}) {
  return createElement(VersionStatus, {
    update,
    loading: false,
    error: "",
    onRetry: props.onRetry ?? (() => undefined),
    status: props.status,
    onApplyingChange: props.onApplyingChange,
  });
}

function dialogText() {
  return byRole(body(), "dialog")
    .map((dialog) => dialog.textContent)
    .join("");
}

async function clickText(root: TestElement, text: string) {
  const button = buttonWithText(root, text);
  if (!button) throw new Error(`button not found: ${text}`);
  await act(async () => {
    button.click();
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
}

/** Starts the update through the confirm flow with the API accepting the POST. */
async function startUpdate(
  status: RuntimeStatus,
  onApplyingChange?: (a: boolean) => void,
) {
  globalThis.fetch = vi
    .fn()
    .mockResolvedValue(
      jsonResponse({ ok: true }, 202),
    ) as unknown as typeof fetch;
  const container = await mount(render({ status, onApplyingChange }));
  await clickText(container, "Update to v0.6.12");
  await clickText(container, "Yes, update now");
  return container;
}

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-10-24T10:00:00Z"));
  // The minimal test DOM lacks a few browser APIs the blocking dialog uses.
  Object.assign(document, { getElementById: () => null });
  Object.assign(window, {
    matchMedia: () => ({
      matches: true,
      addEventListener: () => undefined,
      removeEventListener: () => undefined,
    }),
    scrollTo: () => undefined,
    scrollY: 0,
    location: { reload: vi.fn() },
  });
  Object.assign(Element.prototype, {
    querySelector: () => null,
    querySelectorAll: () => [],
  });
});

afterEach(() => {
  vi.useRealTimers();
});

describe("VersionStatus update flow", () => {
  it("applies a normal update and reaches done when the version changes", async () => {
    const container = await startUpdate({ version: "0.6.11", updating: false });
    expect(dialogText()).toContain("Installing update");

    await rerender(
      container,
      render({
        status: { version: "0.6.11", updating: true, updateTarget: "0.6.12" },
      }),
    );
    expect(dialogText()).toContain("Installing update");

    await rerender(
      container,
      render({ status: { version: "0.6.12", updating: false } }),
    );
    expect(dialogText()).toContain("Update complete");
  });

  it("fails when the update stopped after running but the version did not change", async () => {
    const container = await startUpdate({ version: "0.6.11", updating: false });
    await rerender(
      container,
      render({
        status: { version: "0.6.11", updating: true, updateTarget: "0.6.12" },
      }),
    );
    await rerender(
      container,
      render({ status: { version: "0.6.11", updating: false } }),
    );

    expect(dialogText()).toContain("Update failed");
    expect(dialogText()).toContain("v0.6.11");
    expect(dialogText()).not.toContain("Update complete");
  });

  it("normalizes a leading v when comparing versions", async () => {
    const container = await startUpdate({
      version: "v0.6.11",
      updating: false,
    });
    await rerender(
      container,
      render({
        status: { version: "v0.6.11", updating: true, updateTarget: "0.6.12" },
      }),
    );
    await rerender(
      container,
      render({ status: { version: "0.6.11", updating: false } }),
    );
    expect(dialogText()).toContain("Update failed");
  });

  it("never fails while the updater state is unknown", async () => {
    const container = await startUpdate({ version: "0.6.11", updating: null });
    vi.setSystemTime(new Date("2026-10-24T10:10:00Z"));
    await rerender(
      container,
      render({ status: { version: "0.6.11", updating: null } }),
    );
    await rerender(container, render({ status: undefined }));

    expect(dialogText()).toContain("Installing update");
    expect(dialogText()).not.toContain("Update failed");
  });

  it("waits through the grace period before failing an update never seen running", async () => {
    const container = await startUpdate({ version: "0.6.11", updating: false });

    vi.setSystemTime(new Date("2026-10-24T10:00:05Z"));
    await rerender(
      container,
      render({ status: { version: "0.6.11", updating: false } }),
    );
    expect(dialogText()).toContain("Installing update");

    vi.setSystemTime(new Date("2026-10-24T10:00:16Z"));
    await rerender(
      container,
      render({ status: { version: "0.6.11", updating: false } }),
    );
    expect(dialogText()).toContain("Update failed");
  });

  it("resumes an update that was running before the page loaded, from status alone", async () => {
    const container = await mount(
      render({
        status: { version: "0.6.11", updating: true, updateTarget: "0.6.12" },
      }),
    );
    expect(dialogText()).toContain("Installing update");
    expect(dialogText()).toContain("v0.6.12");

    await rerender(
      container,
      render({ status: { version: "0.6.12", updating: false } }),
    );
    expect(dialogText()).toContain("Update complete");
  });

  it("reports applying to the dashboard so the login redirect can be suppressed", async () => {
    const onApplyingChange = vi.fn();
    const container = await startUpdate(
      { version: "0.6.11", updating: false },
      onApplyingChange,
    );
    expect(onApplyingChange).toHaveBeenLastCalledWith(true);

    await rerender(
      container,
      render({
        status: { version: "0.6.12", updating: false },
        onApplyingChange,
      }),
    );
    expect(onApplyingChange).toHaveBeenLastCalledWith(false);
  });

  it("closes the failed dialog and checks for updates again", async () => {
    const onRetry = vi.fn();
    globalThis.fetch = vi
      .fn()
      .mockResolvedValue(
        jsonResponse({ ok: true }, 202),
      ) as unknown as typeof fetch;
    const container = await mount(
      render({
        status: { version: "0.6.11", updating: true, updateTarget: "0.6.12" },
        onRetry,
      }),
    );
    await rerender(
      container,
      render({ status: { version: "0.6.11", updating: false }, onRetry }),
    );
    expect(dialogText()).toContain("Update failed");

    await clickText(body(), "Close");
    expect(byRole(body(), "dialog")).toHaveLength(0);
    expect(onRetry).toHaveBeenCalled();
  });

  describe("progress bar", () => {
    const running = (
      step: string,
      completed: number,
      total = 5,
    ): RuntimeStatus => ({
      version: "0.6.11",
      updating: true,
      updateTarget: "0.6.12",
      updateProgress: { step, completed, total },
    });

    const bar = () => byRole(body(), "progressbar")[0];
    const spinners = () =>
      body().findAll((el) => el.className.includes("pc-update-spinner"));

    it("shows an accessible determinate bar that advances per step", async () => {
      const container = await mount(render({ status: running("pull", 0) }));
      expect(bar().getAttribute("aria-valuemin")).toBe("0");
      expect(bar().getAttribute("aria-valuemax")).toBe("5");
      expect(bar().getAttribute("aria-valuenow")).toBe("0");
      expect(dialogText()).toContain("Downloading new images");
      expect(dialogText()).toContain("0 of 5");
      expect(spinners()).toHaveLength(0);

      await rerender(container, render({ status: running("restart", 2) }));
      expect(bar().getAttribute("aria-valuenow")).toBe("2");
      expect(dialogText()).toContain("Restarting services");
      expect(dialogText()).toContain("2 of 5");
    });

    it("maps known steps to plain English and shows unknown steps raw", async () => {
      const container = await mount(
        render({ status: running("preflight", 1) }),
      );
      expect(dialogText()).toContain("Checking your installation");
      await rerender(container, render({ status: running("verify", 3) }));
      expect(dialogText()).toContain("Verifying the new version");
      await rerender(container, render({ status: running("bootstrap", 4) }));
      expect(dialogText()).toContain("Finishing up");
      await rerender(container, render({ status: running("mystery", 4) }));
      expect(dialogText()).toContain("mystery");
    });

    it("never moves backwards while applying", async () => {
      const container = await mount(render({ status: running("verify", 3) }));
      await rerender(container, render({ status: running("restart", 2) }));
      expect(bar().getAttribute("aria-valuenow")).toBe("3");
    });

    it("keeps the last known progress when status is missing or updating is null", async () => {
      const container = await mount(render({ status: running("restart", 2) }));
      await rerender(container, render({ status: undefined }));
      expect(bar().getAttribute("aria-valuenow")).toBe("2");
      await rerender(
        container,
        render({ status: { version: "0.6.11", updating: null } }),
      );
      expect(bar().getAttribute("aria-valuenow")).toBe("2");
    });

    it("clamps out-of-range values to 0..total", async () => {
      const container = await mount(render({ status: running("verify", 9) }));
      expect(bar().getAttribute("aria-valuenow")).toBe("5");
      await rerender(container, render({ status: running("verify", -3) }));
      expect(bar().getAttribute("aria-valuenow")).toBe("5");
    });

    it("falls back to the indeterminate spinner without progress", async () => {
      await mount(
        render({
          status: { version: "0.6.11", updating: true, updateTarget: "0.6.12" },
        }),
      );
      expect(byRole(body(), "progressbar")).toHaveLength(0);
      expect(spinners()).toHaveLength(1);
      expect(dialogText()).toContain("Installing update");
    });
  });
});
