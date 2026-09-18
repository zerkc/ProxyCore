import { act, createElement, type ReactElement } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, vi } from "vitest";

export type Listener = (event: TestEvent) => void;

export class TestNode {
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
    index < 0
      ? this.childNodes.push(node)
      : this.childNodes.splice(index, 0, node);
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
    this.listeners[type] = (this.listeners[type] ?? []).filter(
      (item) => item !== listener,
    );
  }

  dispatchEvent(event: TestEvent) {
    event.target ??= this;
    event.currentTarget = this;
    for (const listener of this.listeners[event.type] ?? []) {
      listener.call(this, event);
    }
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

export class TestText extends TestNode {
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

export class TestElement extends TestNode {
  readonly attributes: Record<string, string> = {};
  readonly style = {};
  value = "";
  private _defaultValue = "";
  disabled = false;
  className = "";
  id = "";
  href = "";
  download = "";
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
    if (this.tagName === "TEXTAREA" && this.value === "") {
      this.value = this._defaultValue;
    }
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

  select() {
    this.focus();
  }

  click() {
    this.dispatchEvent(new TestEvent("click", { bubbles: true }));
  }

  remove() {
    if (this.parentNode) this.parentNode.removeChild(this);
  }

  findAll(this: TestElement, predicate: (element: TestElement) => boolean): TestElement[] {
    const matches = predicate(this) ? [this] : [];
    for (const child of this.childNodes) {
      if (child instanceof TestElement) matches.push(...child.findAll(predicate));
    }
    return matches;
  }
}

export class TestDocument extends TestNode {
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

  execCommand(_command: string) {
    return false;
  }
}

export class TestEvent {
  target: TestNode | null = null;
  currentTarget: TestNode | null = null;
  defaultPrevented = false;
  cancelBubble = false;
  readonly bubbles: boolean;

  constructor(
    readonly type: string,
    options: { bubbles?: boolean; cancelable?: boolean } = {},
  ) {
    this.bubbles = options.bubbles ?? false;
  }

  preventDefault() {
    this.defaultPrevented = true;
  }

  stopPropagation() {
    this.cancelBubble = true;
  }
}

export type TestWindow = {
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
type Mounted = { container: TestElement; root: Root };
const mounted: Mounted[] = [];

afterEach(async () => {
  await act(async () => {
    for (const { root } of mounted.splice(0)) root.unmount();
  });
  globalThis.fetch = originalFetch;
});

export function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

export function queueFetch(...responses: Array<Response | Promise<Response>>) {
  const fetchMock = vi.fn();
  for (const response of responses) fetchMock.mockResolvedValueOnce(response);
  globalThis.fetch = fetchMock as unknown as typeof fetch;
  return fetchMock;
}

export function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return { promise, resolve, reject };
}

export function topology(role: "standalone-primary" | "primary" | "primary-with-nodes" | "node" | "stale-primary", writable: boolean) {
  return {
    installationId: "installation",
    nodeId: "node",
    role,
    leadershipGeneration: 1,
    latestKnownGeneration: 1,
    stalePrimary: role === "stale-primary",
    writable,
  } as const;
}

export async function mount(element: ReactElement) {
  const container = document.createElement("div") as unknown as TestElement;
  const root = createRoot(container as unknown as HTMLElement);
  mounted.push({ container, root });
  await act(async () => {
    root.render(element);
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
  return container;
}

export async function rerender(container: TestElement, element: ReactElement) {
  const mountedRoot = mounted.find((item) => item.container === container);
  if (!mountedRoot) return;
  await act(async () => {
    mountedRoot.root.render(element);
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
}

export async function unmount(container: TestElement) {
  const index = mounted.findIndex((item) => item.container === container);
  if (index < 0) return;
  const [mountedRoot] = mounted.splice(index, 1);
  await act(async () => {
    mountedRoot!.root.unmount();
  });
}

export async function settle() {
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
}

export function byRole(container: TestElement, role: string) {
  return container.findAll((element) => element.getAttribute("role") === role);
}

export function byTag(container: TestElement, tagName: string) {
  return container.findAll((element) => element.tagName === tagName);
}

export function byText(container: TestElement, text: string) {
  return container.findAll((element) => element.textContent.includes(text));
}

export function buttonWithText(container: TestElement, text: string) {
  return byTag(container, "BUTTON").find((button) => button.textContent.includes(text));
}

export function formEvent() {
  return new TestEvent("submit", { bubbles: true, cancelable: true });
}

export { createElement };
