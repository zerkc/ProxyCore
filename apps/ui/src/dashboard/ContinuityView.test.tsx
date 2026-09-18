import { act, createElement } from "react";
import { createRoot } from "react-dom/client";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ContinuityView } from "./ContinuityView";
import type { TopologyIdentity } from "./types";
type Listener = (event: TestEvent) => void;
class TestNode {
  readonly childNodes: TestNode[] = [];
  parentNode: TestNode | null = null;
  private readonly listeners: Record<string, Listener[]> = {};

  constructor(
    readonly nodeType: number,
    readonly nodeName: string,
    readonly ownerDocument: TestDocument | null,
  ) {}

  appendChild(node: TestNode) {
    if (node.parentNode) node.parentNode.removeChild(node);
    this.childNodes.push(node);
    node.parentNode = this;
    return node;
  }
  insertBefore(node: TestNode, before: TestNode | null) {
    if (node.parentNode) node.parentNode.removeChild(node);
    const index = before ? this.childNodes.indexOf(before) : -1;
    index < 0 ? this.childNodes.push(node) : this.childNodes.splice(index, 0, node);
    node.parentNode = this;
    return node;
  }

  removeChild(node: TestNode) {
    const index = this.childNodes.indexOf(node);
    if (index < 0) throw new Error("child not found");
    this.childNodes.splice(index, 1);
    node.parentNode = null;
    return node;
  }
  addEventListener(type: string, listener: Listener) {
    (this.listeners[type] ??= []).push(listener);
  }

  removeEventListener(type: string, listener: Listener) {
    this.listeners[type] = (this.listeners[type] ?? []).filter((item) => item !== listener);
  }

  dispatchEvent(event: TestEvent) {
    event.target ??= this;
    event.currentTarget = this;
    for (const listener of this.listeners[event.type] ?? []) listener.call(this, event);
    if (event.bubbles && !event.cancelBubble && this.parentNode) {
      this.parentNode.dispatchEvent(event);
    }
    return !event.defaultPrevented;
  }
  contains(node: TestNode): boolean {
    return node === this || this.childNodes.some((child) => child.contains(node));
  }

  get firstChild() {
    return this.childNodes[0] ?? null;
  }
  get textContent() {
    return this.childNodes.map((node) => node.textContent).join("");
  }

  set textContent(value: string) {
    for (const child of this.childNodes) child.parentNode = null;
    this.childNodes.length = 0;
    if (value) this.appendChild(new TestText(value, this.ownerDocument));
  }
}
class TestText extends TestNode {
  data: string;

  constructor(data: string, ownerDocument: TestDocument | null) {
    super(3, "#text", ownerDocument);
    this.data = data;
  }

  get textContent() {
    return this.data;
  }

  set textContent(value: string) {
    this.data = value;
  }
}
class TestElement extends TestNode {
  readonly attributes: Record<string, string> = {};
  readonly style = {};
  value = "";
  private _defaultValue = "";
  disabled = false;
  className = "";
  id = "";
  tagName: string;

  constructor(name: string, ownerDocument: TestDocument) {
    super(1, name.toUpperCase(), ownerDocument);
    this.tagName = this.nodeName;
  }
  get defaultValue() {
    return this._defaultValue;
  }

  set defaultValue(value: string) {
    this._defaultValue = String(value);
    if (this.tagName === "TEXTAREA" && this.value === "") this.value = this._defaultValue;
  }

  setAttribute(name: string, value: string) {
    this.attributes[name] = String(value);
    if (name === "id") this.id = String(value);
    if (name === "class") this.className = String(value);
    if (name === "disabled") this.disabled = true;
  }

  removeAttribute(name: string) {
    delete this.attributes[name];
    if (name === "disabled") this.disabled = false;
  }

  getAttribute(name: string) {
    return this.attributes[name] ?? null;
  }
  focus() {
    if (this.ownerDocument) this.ownerDocument.activeElement = this;
  }

  findAll(this: TestElement, predicate: (element: TestElement) => boolean): TestElement[] {
    const matches = predicate(this) ? [this] : [];
    for (const child of this.childNodes) {
      if (child instanceof TestElement) matches.push(...child.findAll(predicate));
    }
    return matches;
  }
}
class TestDocument extends TestNode {
  readonly documentElement: TestElement;
  readonly body: TestElement;
  activeElement: TestElement;
  defaultView: TestWindow | null = null;

  constructor() {
    super(9, "#document", null);
    this.documentElement = new TestElement("html", this);
    this.body = new TestElement("body", this);
    this.activeElement = this.body;
    this.documentElement.appendChild(this.body);
  }

  createElement(name: string) {
    return new TestElement(name, this);
  }

  createElementNS(_namespace: string, name: string) {
    return new TestElement(name, this);
  }

  createTextNode(value: string) {
    return new TestText(value, this);
  }
}

class TestEvent {
  target: TestNode | null = null;
  currentTarget: TestNode | null = null;
  defaultPrevented = false;
  cancelBubble = false;
  readonly bubbles: boolean;

  constructor(readonly type: string, options: { bubbles?: boolean; cancelable?: boolean } = {}) {
    this.bubbles = options.bubbles ?? false;
  }

  preventDefault() {
    this.defaultPrevented = true;
  }

  stopPropagation() {
    this.cancelBubble = true;
  }
}

type TestWindow = {
  document: TestDocument;
  HTMLIFrameElement: typeof TestElement;
  addEventListener: () => void;
  removeEventListener: () => void;
};

const testDocument = new TestDocument();
const testWindow: TestWindow = {
  document: testDocument,
  HTMLIFrameElement: TestElement,
  addEventListener: () => undefined,
  removeEventListener: () => undefined,
};
testDocument.defaultView = testWindow;
Object.assign(globalThis, {
  document: testDocument,
  window: testWindow,
  Node: TestNode,
  Element: TestElement,
  HTMLElement: TestElement,
  HTMLIFrameElement: TestElement,
  HTMLInputElement: TestElement,
  HTMLTextAreaElement: TestElement,
  HTMLButtonElement: TestElement,
  HTMLFormElement: TestElement,
  Event: TestEvent,
  IS_REACT_ACT_ENVIRONMENT: true,
});

const originalFetch = globalThis.fetch;
const mounted: Array<{ unmount: () => void }> = [];

afterEach(async () => {
  await act(async () => {
    for (const root of mounted.splice(0)) root.unmount();
  });
  globalThis.fetch = originalFetch;
});

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

function queueContinuityFetch(
  hostnames: Array<Response | Promise<Response>>,
  trust: Array<Response | Promise<Response>>,
) {
  let hostnameIndex = 0;
  let trustIndex = 0;
  const fetchMock = vi.fn((input: RequestInfo | URL) => {
    const path = String(input);
    return path.includes("/enrollment-trust")
      ? trust[trustIndex++]
      : hostnames[hostnameIndex++];
  });
  globalThis.fetch = fetchMock as unknown as typeof fetch;
  return fetchMock;
}

function topology(role: TopologyIdentity["role"], writable: boolean): TopologyIdentity {
  return {
    installationId: "installation",
    nodeId: "node",
    role,
    leadershipGeneration: 1,
    latestKnownGeneration: 1,
    stalePrimary: role === "stale-primary",
    writable,
  };
}

async function mount(identity: TopologyIdentity) {
  const container = document.createElement("div") as unknown as TestElement;
  const root = createRoot(container as unknown as HTMLElement);
  mounted.push(root);
  await act(async () => {
    root.render(
      createElement(
        MemoryRouter,
        null,
        createElement(ContinuityView, { identity }),
      ),
    );
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
  return container;
}

function byRole(container: TestElement, role: string) {
  return container.findAll((element) => element.getAttribute("role") === role);
}

function byTag(container: TestElement, tagName: string) {
  return container.findAll((element) => element.tagName === tagName);
}

async function submit(container: TestElement) {
  const form = byTag(container, "FORM")[0]!;
  await act(async () => {
    form.dispatchEvent(new TestEvent("submit", { bubbles: true, cancelable: true }));
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
}

describe("mounted ContinuityView", () => {
  it("renders an accessible loading status while GET is pending", async () => {
    const fetchMock = queueContinuityFetch(
      [new Promise<Response>(() => {})],
      [new Promise<Response>(() => {})],
    );
    const container = await mount(topology("standalone-primary", true));

    expect(byRole(container, "status")[0]!.textContent).toContain("Loading");
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/settings/enrollment-hostnames",
      expect.objectContaining({ cache: "no-store", credentials: "include" }),
    );
  });

  it("renders unconfigured and configured states with an accessible hostname label", async () => {
    queueContinuityFetch(
      [jsonResponse({ configured: false, hostnames: [] })],
      [jsonResponse({ status: "unconfigured", configured: false, ready: false })],
    );
    const unconfigured = await mount(topology("standalone-primary", true));
    expect(unconfigured.textContent).toContain("Not configured");
    expect(unconfigured.textContent).toContain("TLS SANs for enrollment on port 3443");
    expect(byTag(unconfigured, "LABEL")[0]!.textContent).toContain("DNS names or IP literals");
    expect(byTag(unconfigured, "TEXTAREA")[0]!.getAttribute("id")).toBe("enrollment-hostnames");

    queueContinuityFetch(
      [jsonResponse({ configured: true, hostnames: ["primary.example"] })],
      [jsonResponse({ status: "not-ready", configured: true, ready: false })],
    );
    const configured = await mount(topology("standalone-primary", true));
    expect(configured.textContent).toContain("Configured");
    expect(byTag(configured, "TEXTAREA")[0]!.value).toBe("primary.example");
  });

  it("disables editing and saving for NODE and stale-primary identities", async () => {
    for (const role of ["node", "stale-primary"] as const) {
      queueContinuityFetch(
        [jsonResponse({ configured: true, hostnames: ["primary.example"] })],
        [],
      );
      const container = await mount(topology(role, false));
      expect(byTag(container, "TEXTAREA")[0]!.disabled).toBe(true);
      expect(byTag(container, "BUTTON")[0]!.disabled).toBe(true);
      expect(container.textContent).toContain("cannot change it");
    }
  });

  it("renders the canonical PUT response and success status", async () => {
    const fetchMock = queueContinuityFetch(
      [
        jsonResponse({ configured: true, hostnames: ["Input.Example"] }),
        jsonResponse({ configured: true, hostnames: ["10.0.0.5", "input.example"] }),
      ],
      [jsonResponse({ status: "not-ready", configured: true, ready: false })],
    );
    const container = await mount(topology("standalone-primary", true));

    await submit(container);

    const textarea = byTag(container, "TEXTAREA")[0]!;
    expect(textarea.value).toBe("10.0.0.5\ninput.example");
    expect(byRole(container, "status").map((element) => element.textContent)).toContain(
      "Saved. The canonical server response is shown above.",
    );
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/settings/enrollment-hostnames",
      expect.objectContaining({
        method: "PUT",
        body: JSON.stringify({ hostnames: ["Input.Example"] }),
      }),
    );
  });

  it("renders validation failures as an accessible alert", async () => {
    queueContinuityFetch(
      [
        jsonResponse({ configured: true, hostnames: ["primary.example"] }),
        jsonResponse({ error: "invalid enrollment DNS name" }, 400),
      ],
      [jsonResponse({ status: "not-ready", configured: true, ready: false })],
    );
    const container = await mount(topology("standalone-primary", true));

    await submit(container);

    expect(byRole(container, "alert")[0]!.textContent).toBe("invalid enrollment DNS name");
  });

  it("renders no Owner controls when the Owner GET is forbidden", async () => {
    queueContinuityFetch(
      [jsonResponse({ error: "Permission denied" }, 403)],
      [jsonResponse({ error: "Permission denied" }, 403)],
    );
    const container = await mount(topology("standalone-primary", true));

    expect(byRole(container, "alert")[0]!.textContent).toContain("Owner access is required");
    expect(byTag(container, "TEXTAREA")).toHaveLength(0);
  });
});
