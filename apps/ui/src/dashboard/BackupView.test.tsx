import { act, createElement } from "react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { SESSION_USERNAME_KEY } from "../api";
import { BackupView } from "./BackupView";
import {
  buttonWithText,
  byRole,
  deferred,
  jsonResponse,
  mount,
  queueFetch,
  settle,
  TestEvent,
  type TestElement,
} from "./testDom";

const report = {
  dryRun: true,
  bundleSha256: "ab".repeat(32),
  formatVersion: 1,
  exporterVersion: "0.5.2",
  createdAt: "2030-01-01T00:00:00Z",
  installationId: "inst-1",
  nodeId: "node-1",
  tables: [
    { name: "zones", rowCount: 3, action: "replace" },
    { name: "records", rowCount: 12, action: "replace" },
  ],
  envWouldChange: true,
  certsToRestore: ["a.example", "b.example"],
  appliedPostImport: false,
};

function view() {
  return createElement(
    MemoryRouter,
    { initialEntries: ["/dashboard/backup"] },
    createElement(
      Routes,
      null,
      createElement(Route, {
        path: "/dashboard/backup",
        element: createElement(BackupView),
      }),
      createElement(Route, {
        path: "/login",
        element: createElement("p", { "data-testid": "login-page" }, "login"),
      }),
    ),
  );
}

function byId(container: TestElement, id: string) {
  return container.findAll((element) => element.id === id)[0]!;
}

function byTestId(container: TestElement, id: string) {
  return container.findAll(
    (element) => element.getAttribute("data-testid") === id,
  )[0];
}

// The minimal test DOM lacks `oninput` support, so React falls back to its
// legacy polyfill (focusin to start watching, keyup to read the value). Drive
// that path so `PasswordInput`'s onChange fires.
function setValue(element: TestElement, value: string) {
  act(() => {
    Object.assign(element, {
      type: "password",
      attachEvent: () => undefined,
      detachEvent: () => undefined,
    });
    element.dispatchEvent(new TestEvent("focusin", { bubbles: true }));
    element.value = value;
    element.dispatchEvent(new TestEvent("keyup", { bubbles: true }));
  });
}

function pickFile(container: TestElement, file: File) {
  const input = byId(container, "backup-import-file") as TestElement & {
    files?: File[];
  };
  act(() => {
    Object.defineProperty(input, "files", {
      configurable: true,
      value: [file],
    });
    input.dispatchEvent(new TestEvent("change", { bubbles: true }));
  });
}

async function click(container: TestElement, text: string) {
  const button = buttonWithText(container, text);
  expect(button, `button ${text}`).toBeDefined();
  await act(async () => {
    button!.dispatchEvent(new TestEvent("click", { bubbles: true }));
    await settle();
  });
}

const store = new Map<string, string>();

beforeEach(() => {
  store.clear();
  Object.defineProperty(globalThis, "sessionStorage", {
    configurable: true,
    value: {
      getItem: (key: string) => store.get(key) ?? null,
      setItem: (key: string, value: string) => void store.set(key, value),
      removeItem: (key: string) => void store.delete(key),
    },
  });
  Object.defineProperty(URL, "createObjectURL", {
    configurable: true,
    value: vi.fn(() => "blob:test"),
  });
  Object.defineProperty(URL, "revokeObjectURL", {
    configurable: true,
    value: vi.fn(),
  });
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe("BackupView export", () => {
  it("downloads the bundle with the server filename and reports success", async () => {
    const fetchMock = queueFetch(
      new Response("zip", {
        status: 200,
        headers: {
          "content-disposition": 'attachment; filename="backup-1.zip"',
        },
      }),
    );
    const container = await mount(view());
    const clicked: string[] = [];
    const created = document.createElement.bind(document);
    vi.spyOn(document, "createElement").mockImplementation((name: string) => {
      const element = created(name) as TestElement;
      element.click = () => void clicked.push(element.download);
      return element as unknown as HTMLElement;
    });

    setValue(byId(container, "backup-export-passphrase"), "s3cret");
    await click(container, "Export backup");

    const [url, init] = fetchMock.mock.calls[0]! as [string, RequestInit];
    expect(url).toBe("/api/backup/export");
    expect(
      (init.headers as Record<string, string>)["X-Proxycore-Backup-Passphrase"],
    ).toBeTruthy();
    expect(clicked).toEqual(["backup-1.zip"]);
    expect(byRole(container, "status")[0]!.textContent).toContain(
      "Backup exported",
    );
    expect(container.textContent).not.toContain("s3cret");
  });

  it("disables export while the request is in flight", async () => {
    const pending = deferred<Response>();
    queueFetch(pending.promise);
    const container = await mount(view());

    await click(container, "Export backup");

    expect(buttonWithText(container, "Exporting")!.disabled).toBe(true);
    await act(async () => {
      pending.resolve(new Response("zip", { status: 200 }));
      await settle();
    });
    expect(buttonWithText(container, "Export backup")!.disabled).toBe(false);
  });

  it.each([
    [403, { error: "forbidden" }, "Owner access is required"],
    [401, { error: "unauthorized" }, "Owner access is required"],
    [
      503,
      { error: "backup service is not configured" },
      "master key is not configured",
    ],
  ])("maps export status %i to a clear message", async (status, body, text) => {
    queueFetch(jsonResponse(body, status));
    const container = await mount(view());

    await click(container, "Export backup");

    expect(byRole(container, "alert")[0]!.textContent).toContain(text);
  });
});

describe("BackupView import", () => {
  async function previewed() {
    const fetchMock = queueFetch(jsonResponse(report));
    const container = await mount(view());
    pickFile(container, new File(["zip"], "b.zip"));
    await click(container, "Preview import");
    return { container, fetchMock };
  }

  it("requires a file before previewing", async () => {
    const container = await mount(view());

    expect(buttonWithText(container, "Preview import")!.disabled).toBe(true);
    expect(buttonWithText(container, "Restore backup")).toBeUndefined();
  });

  it("runs a dry-run and renders metadata, tables and warnings", async () => {
    const { container, fetchMock } = await previewed();

    expect(fetchMock.mock.calls[0]![0]).toBe("/api/backup/import?dry_run=true");
    const text = container.textContent;
    for (const expected of [
      "zones",
      "records",
      "12",
      "replace",
      "0.5.2",
      "node-1",
      "2030-01-01T00:00:00Z",
      "Environment changes",
      "2 certificates",
      "replaces all configuration",
    ]) {
      expect(text).toContain(expected);
    }
    expect(buttonWithText(container, "Restore backup")).toBeDefined();
  });

  it("sends the import passphrase base64url-encoded in the form body", async () => {
    const fetchMock = queueFetch(jsonResponse(report));
    const container = await mount(view());
    pickFile(container, new File(["zip"], "b.zip"));
    setValue(byId(container, "backup-import-passphrase"), "hunter2");

    await click(container, "Preview import");

    const init = fetchMock.mock.calls[0]![1] as RequestInit;
    const form = init.body as FormData;
    expect(form.get("passphrase")).toBe("aHVudGVyMg");
    expect(fetchMock.mock.calls[0]![0]).not.toContain("hunter2");
  });

  it("shows compatibility conflicts from the preview", async () => {
    queueFetch(
      jsonResponse({
        ...report,
        compatibilityConflicts: ["schema drift in zones"],
      }),
    );
    const container = await mount(view());
    pickFile(container, new File(["zip"], "b.zip"));
    await click(container, "Preview import");

    expect(container.textContent).toContain("schema drift in zones");
  });

  it("restores with dry_run=false, clears the session, and goes to login", async () => {
    store.set(SESSION_USERNAME_KEY, "owner");
    const { container, fetchMock } = await previewed();
    fetchMock.mockResolvedValueOnce(jsonResponse({ ...report, dryRun: false }));

    await click(container, "Restore backup");

    expect(fetchMock.mock.calls[1]![0]).toBe(
      "/api/backup/import?dry_run=false",
    );
    expect(store.has(SESSION_USERNAME_KEY)).toBe(false);
    expect(byTestId(container, "login-page")).toBeDefined();
  });

  it("drops the preview when the file changes", async () => {
    const { container } = await previewed();

    pickFile(container, new File(["other"], "c.zip"));

    expect(buttonWithText(container, "Restore backup")).toBeUndefined();
  });

  it.each([
    [
      409,
      { code: "master_key_mismatch", error: "master key mismatch" },
      "different master key",
    ],
    [401, { error: "passphrase mismatch" }, "passphrase is incorrect"],
    [400, { error: "passphrase required" }, "Enter its passphrase"],
    [413, { error: "backup bundle is too large" }, "too large"],
    [
      503,
      { error: "backup service is not configured" },
      "master key is not configured",
    ],
    [403, { error: "forbidden" }, "Owner access is required"],
    [400, { error: "invalid backup archive" }, "not a valid ProxyCore backup"],
  ])("maps import status %i to a clear message", async (status, body, text) => {
    queueFetch(jsonResponse(body, status));
    const container = await mount(view());
    pickFile(container, new File(["zip"], "b.zip"));

    await click(container, "Preview import");

    expect(byRole(container, "alert")[0]!.textContent).toContain(text);
    expect(buttonWithText(container, "Restore backup")).toBeUndefined();
  });

  it("disables both import actions while restoring", async () => {
    const { container, fetchMock } = await previewed();
    const pending = deferred<Response>();
    fetchMock.mockReturnValueOnce(pending.promise);

    await click(container, "Restore backup");

    expect(buttonWithText(container, "Restoring")!.disabled).toBe(true);
    expect(buttonWithText(container, "Preview import")!.disabled).toBe(true);
    await act(async () => {
      pending.reject(new TypeError("offline"));
      await settle();
    });
    expect(byRole(container, "alert")[0]).toBeDefined();
  });
});
