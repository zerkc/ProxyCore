import type {
  EnrollmentHostnameConfig,
  EnrollmentTrust,
  EnrollmentTrustDownload,
  TopologyNodeList,
} from "./dashboard/types";
import type {
  ConfirmResult,
  DraftPreview,
  EnrollmentTokenCreation,
  EnrollmentTokenDefaults,
  EnrollmentTokenSummary,
  RecoverableState,
  TopologyConversionResult,
} from "./types";

export type {
  ConfirmResult,
  DraftPreview,
  EnrollmentTokenCreation,
  EnrollmentTokenDefaults,
  EnrollmentTokenSummary,
  RecoverableState,
  TopologyConversionResult,
} from "./types";

export const SESSION_USERNAME_KEY = "proxycore_username";

export class ApiError extends Error {
  constructor(
    public readonly status: number,
    message: string,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

export type EnrollmentApiErrorKind =
  | "unauthorized"
  | "forbidden"
  | "bad-request"
  | "not-found"
  | "conflict"
  | "gone"
  | "server"
  | "network"
  | "invalid-response"
  | "payload-too-large"
  | "untrusted-certificate"
  | "invalid-ca"
  | "trust-not-ready"
  | "trust-unavailable"
  | "primary-url-required"
  | "primary-url-invalid"
  | "primary-url-not-in-cert";

export class EnrollmentApiError extends ApiError {
  constructor(
    status: number,
    public readonly kind: EnrollmentApiErrorKind,
    message: string,
  ) {
    super(status, message);
    this.name = "EnrollmentApiError";
  }
}

export type PublicUser = {
  id: string;
  username: string;
  role: "owner" | "operator";
  active: boolean;
  passwordChangeRequired: boolean;
};

export async function api<T>(path: string, init: RequestInit = {}): Promise<T> {
  const response = await fetch(path, {
    credentials: "include",
    headers: {
      "content-type": "application/json",
      ...(init.headers ?? {}),
    },
    ...init,
  });
  if (response.status === 204) {
    return undefined as T;
  }
  if (!response.ok) {
    throw await responseApiError(response);
  }
  return (await response.json()) as T;
}

async function responseApiError(response: Response) {
  const body = (await response.json().catch(() => ({}))) as {
    error?: unknown;
  };
  const message =
    typeof body.error === "string" && body.error
      ? body.error
      : `Request failed (${response.status})`;
  return new ApiError(response.status, message);
}

export function getEnrollmentHostnames(signal?: AbortSignal) {
  return api<EnrollmentHostnameConfig>(
    "/api/settings/enrollment-hostnames",
    signal ? { cache: "no-store", signal } : { cache: "no-store" },
  );
}

export function updateEnrollmentHostnames(hostnames: string[]) {
  return api<EnrollmentHostnameConfig>(
    "/api/settings/enrollment-hostnames",
    {
      method: "PUT",
      body: JSON.stringify({ hostnames }),
    },
  );
}

export function getEnrollmentTrust(signal?: AbortSignal) {
  return api<EnrollmentTrust>(
    "/api/settings/enrollment-trust",
    signal ? { cache: "no-store", signal } : { cache: "no-store" },
  );
}

const ENROLLMENT_CA_FILENAME = "proxycore-enrollment-ca.pem";

export async function downloadEnrollmentTrustCA(): Promise<EnrollmentTrustDownload> {
  const response = await fetch("/api/settings/enrollment-trust/ca", {
    credentials: "include",
    cache: "no-store",
  });
  if (!response.ok) {
    throw await responseApiError(response);
  }
  return {
    blob: await response.blob(),
    filename: ENROLLMENT_CA_FILENAME,
  };
}

const ENROLLMENT_REQUEST_TIMEOUT_MS = 10_000;
const ENROLLMENT_RESPONSE_LIMIT_BYTES = 64 * 1024;

type EnrollmentOperation =
  | "create-token"
  | "token-defaults"
  | "list-tokens"
  | "revoke-token"
  | "draft"
  | "preview"
  | "confirm"
  | "recover";

export async function createEnrollmentToken(
  options?: { primaryUrl?: string } | AbortSignal,
  signal?: AbortSignal,
): Promise<EnrollmentTokenCreation> {
  // Backwards compatible: the first argument used to be the abort signal.
  const resolvedOptions = options instanceof AbortSignal ? undefined : options;
  const resolvedSignal = options instanceof AbortSignal ? options : signal;
  const primaryUrl = resolvedOptions?.primaryUrl?.trim();
  const body = await enrollmentRequest<unknown>(
    "create-token",
    "/api/enrollment/tokens",
    { method: "POST", body: JSON.stringify(primaryUrl ? { primaryUrl } : {}) },
    resolvedSignal,
  );
  return normalizeTokenCreation(body);
}

export function getEnrollmentTokenDefaults(
  signal?: AbortSignal,
): Promise<EnrollmentTokenDefaults> {
  return enrollmentRequest<EnrollmentTokenDefaults>(
    "token-defaults",
    "/api/enrollment/tokens/defaults",
    { method: "GET" },
    signal,
  );
}

export function listEnrollmentTokens(
  signal?: AbortSignal,
): Promise<EnrollmentTokenSummary[]> {
  return enrollmentRequest<EnrollmentTokenSummary[]>(
    "list-tokens",
    "/api/enrollment/tokens",
    { method: "GET" },
    signal,
  );
}

export function revokeEnrollmentToken(
  id: string,
  confirm: "replace-and-revoke",
  signal?: AbortSignal,
): Promise<void> {
  const path = `/api/enrollment/tokens/${encodeURIComponent(id)}/revoke?confirm=${encodeURIComponent(confirm)}`;
  return enrollmentRequest<void>(
    "revoke-token",
    path,
    { method: "POST" },
    signal,
  );
}

export function draftEnrollment(
  token: string,
  primaryURL?: string,
  signal?: AbortSignal,
  caPem?: string,
): Promise<DraftPreview> {
  return enrollmentRequest<DraftPreview>(
    "draft",
    "/api/topology/enrollment/draft",
    {
      method: "POST",
      body: JSON.stringify({
        token,
        ...(primaryURL ? { primaryURL } : {}),
        ...(caPem ? { caPem } : {}),
      }),
    },
    signal,
  );
}

export function previewEnrollment(
  draftId: string,
  requestedNodeID?: string,
  requestedIngress?: string,
  signal?: AbortSignal,
): Promise<DraftPreview> {
  return enrollmentRequest<DraftPreview>(
    "preview",
    "/api/topology/enrollment/preview",
    {
      method: "POST",
      body: JSON.stringify({
        draftId,
        ...(requestedNodeID ? { requestedNodeID } : {}),
        ...(requestedIngress
          ? { requestedIngress: enrollmentIngressPayload(requestedIngress) }
          : {}),
      }),
    },
    signal,
  );
}

export function confirmEnrollment(
  draftId: string,
  signal?: AbortSignal,
): Promise<ConfirmResult> {
  return enrollmentRequest<ConfirmResult>(
    "confirm",
    "/api/topology/enrollment/confirm",
    { method: "POST", body: JSON.stringify({ draftId }) },
    signal,
  );
}

export async function recoverEnrollment(
  token?: string,
  signal?: AbortSignal,
): Promise<RecoverableState | "token-missing"> {
  try {
    return await enrollmentRequest<RecoverableState>(
      "recover",
      "/api/topology/enrollment/recover",
      {
        method: "POST",
        body: JSON.stringify(token ? { token } : {}),
      },
      signal,
    );
  } catch (caught) {
    if (caught instanceof EnrollmentApiError && caught.status === 410) {
      return "token-missing";
    }
    throw caught;
  }
}

async function enrollmentRequest<T>(
  operation: EnrollmentOperation,
  path: string,
  init: RequestInit,
  signal?: AbortSignal,
): Promise<T> {
  let response: Response;
  try {
    response = await fetch(path, {
      ...init,
      credentials: "include",
      cache: "no-store",
      redirect: "error",
      headers: {
        "content-type": "application/json",
        ...(init.headers ?? {}),
      },
      signal: enrollmentSignal(signal),
    });
  } catch {
    throw new EnrollmentApiError(
      0,
      "network",
      "Enrollment service is unavailable.",
    );
  }

  if (!response.ok) {
    let text = "";
    try {
      text = await readBoundedText(response);
    } catch {
      // Error response contents are intentionally discarded, including secrets.
    }
    const known = draftTrustErrorKind(operation, response.status, text);
    if (known) throw new EnrollmentApiError(response.status, known, draftTrustErrorMessages[known]);
    const tokenKnown = tokenCreateErrorKind(operation, response.status, text);
    if (tokenKnown) throw new EnrollmentApiError(response.status, tokenKnown.kind, tokenKnown.message);
    throw enrollmentStatusError(operation, response.status);
  }
  if (response.status === 204) return undefined as T;

  let text: string;
  try {
    text = await readBoundedText(response);
  } catch (caught) {
    if (caught instanceof EnrollmentApiError) throw caught;
    throw new EnrollmentApiError(
      response.status,
      "invalid-response",
      "Enrollment service returned an invalid response.",
    );
  }
  if (!text.trim()) return undefined as T;
  try {
    return JSON.parse(text) as T;
  } catch {
    throw new EnrollmentApiError(
      response.status,
      "invalid-response",
      "Enrollment service returned an invalid response.",
    );
  }
}

function enrollmentSignal(signal?: AbortSignal) {
  const timeout = AbortSignal.timeout(ENROLLMENT_REQUEST_TIMEOUT_MS);
  if (!signal) return timeout;
  if (signal.aborted) return signal;
  return AbortSignal.any([signal, timeout]);
}

async function readBoundedText(response: Response) {
  if (!response.body) {
    const text = await response.text();
    if (new TextEncoder().encode(text).byteLength > ENROLLMENT_RESPONSE_LIMIT_BYTES) {
      throw new EnrollmentApiError(
        response.status,
        "payload-too-large",
        "Enrollment service returned an oversized response.",
      );
    }
    return text;
  }
  const reader = response.body.getReader();
  const chunks: Uint8Array[] = [];
  let length = 0;
  try {
    while (true) {
      const result = await reader.read();
      if (result.done) break;
      length += result.value.byteLength;
      if (length > ENROLLMENT_RESPONSE_LIMIT_BYTES) {
        throw new EnrollmentApiError(
          response.status,
          "payload-too-large",
          "Enrollment service returned an oversized response.",
        );
      }
      chunks.push(result.value);
    }
  } finally {
    reader.releaseLock();
  }
  const bytes = new Uint8Array(length);
  let offset = 0;
  for (const chunk of chunks) {
    bytes.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return new TextDecoder().decode(bytes);
}

// The only error bodies the UI reads: two fixed strings from the draft route
// that tell the owner to fix the CA, each tied to its exact status. Anything
// else stays discarded.
const draftTrustErrors: Record<string, { status: number; kind: "untrusted-certificate" | "invalid-ca" }> = {
  "primary certificate not trusted; load the primary CA": { status: 409, kind: "untrusted-certificate" },
  "invalid CA certificate": { status: 400, kind: "invalid-ca" },
};
const draftTrustErrorMessages = {
  "untrusted-certificate": "The PRIMARY certificate is not trusted by this NODE.",
  "invalid-ca": "The CA certificate is not valid.",
} as const;

// Token creation reports its failures with a stable machine code; only these
// code/status pairs are read, everything else stays discarded.
const tokenCreateErrors: Record<string, { status: number; kind: EnrollmentApiErrorKind; message: string }> = {
  ENROLLMENT_TRUST_NOT_READY: { status: 409, kind: "trust-not-ready", message: "Enrollment trust is not ready." },
  ENROLLMENT_TRUST_UNAVAILABLE: { status: 503, kind: "trust-unavailable", message: "Enrollment trust is unavailable." },
  PRIMARY_URL_REQUIRED: { status: 422, kind: "primary-url-required", message: "A primary URL is required." },
  PRIMARY_URL_INVALID: { status: 422, kind: "primary-url-invalid", message: "The primary URL is not valid." },
  PRIMARY_URL_NOT_IN_CERT: { status: 422, kind: "primary-url-not-in-cert", message: "The primary URL host is not covered by the enrollment certificate." },
};

function tokenCreateErrorKind(operation: EnrollmentOperation, status: number, text: string) {
  if (operation !== "create-token") return undefined;
  try {
    const parsed = JSON.parse(text) as { code?: unknown };
    if (typeof parsed.code !== "string" || !Object.hasOwn(tokenCreateErrors, parsed.code)) return undefined;
    const known = tokenCreateErrors[parsed.code]!;
    return known.status === status ? known : undefined;
  } catch {
    return undefined;
  }
}

function draftTrustErrorKind(operation: EnrollmentOperation, status: number, text: string) {
  if (operation !== "draft") return undefined;
  try {
    const parsed = JSON.parse(text) as { error?: unknown };
    if (typeof parsed.error !== "string" || !Object.hasOwn(draftTrustErrors, parsed.error)) return undefined;
    const known = draftTrustErrors[parsed.error]!;
    return known.status === status ? known.kind : undefined;
  } catch {
    return undefined;
  }
}

function enrollmentStatusError(operation: EnrollmentOperation, status: number) {
  const kind: EnrollmentApiErrorKind =
    status === 401
      ? "unauthorized"
      : status === 403
        ? "forbidden"
        : status === 400
          ? "bad-request"
          : status === 404
            ? "not-found"
            : status === 409
              ? "conflict"
              : status === 410
                ? "gone"
                : status >= 500
                  ? "server"
                  : "network";
  const messages: Record<EnrollmentOperation, Partial<Record<EnrollmentApiErrorKind, string>>> = {
    "create-token": {
      unauthorized: "Sign in again to create an enrollment token.",
      forbidden: "Owner access is required to create an enrollment token.",
      conflict: "The enrollment token could not be created.",
      server: "Enrollment token creation is temporarily unavailable.",
    },
    "token-defaults": {
      unauthorized: "Sign in again to load enrollment defaults.",
      forbidden: "Owner access is required to load enrollment defaults.",
      server: "Enrollment defaults are temporarily unavailable.",
    },
    "list-tokens": {
      unauthorized: "Sign in again to view enrollment tokens.",
      forbidden: "Owner access is required to view enrollment tokens.",
      server: "Enrollment tokens are temporarily unavailable.",
    },
    "revoke-token": {
      unauthorized: "Sign in again to revoke an enrollment token.",
      forbidden: "Owner access is required to revoke enrollment tokens.",
      conflict: "The enrollment token could not be revoked.",
      server: "Enrollment token revocation is temporarily unavailable.",
    },
    draft: {
      "bad-request": "The enrollment draft request is invalid.",
      forbidden: "The enrollment token was rejected.",
      gone: "The enrollment token is no longer available.",
      server: "The enrollment preview is temporarily unavailable.",
    },
    preview: {
      "not-found": "The enrollment draft could not be found.",
      gone: "The enrollment draft has expired.",
      server: "The enrollment preview is temporarily unavailable.",
    },
    confirm: {
      conflict: "Enrollment confirmation was rejected.",
      gone: "The enrollment draft has expired.",
      server: "Enrollment confirmation is temporarily unavailable.",
    },
    recover: {
      gone: "Please re-enter your enrollment token.",
      server: "Enrollment recovery is temporarily unavailable.",
    },
  };
  return new EnrollmentApiError(
    status,
    kind,
    messages[operation][kind] ?? "The enrollment request could not be completed.",
  );
}

function enrollmentIngressPayload(value: string) {
  return value.includes(":") ? { ipv6: value } : { ipv4: value };
}

function normalizeTokenCreation(value: unknown): EnrollmentTokenCreation {
  if (!isRecord(value)) {
    throw new EnrollmentApiError(
      200,
      "invalid-response",
      "Enrollment service returned an invalid token response.",
    );
  }
  if (typeof value.token === "string") {
    return value as unknown as EnrollmentTokenCreation;
  }
  if (
    typeof value.Token === "string" &&
    typeof value.ID === "string" &&
    typeof value.Selector === "string" &&
    typeof value.ExpiresAt === "string"
  ) {
    return {
      id: value.ID,
      selector: value.Selector,
      token: value.Token,
      expiresAt: value.ExpiresAt,
    };
  }
  throw new EnrollmentApiError(
    200,
    "invalid-response",
    "Enrollment service returned an invalid token response.",
  );
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

export type TopologyApiErrorKind =
  | "unauthorized"
  | "forbidden"
  | "bad-request"
  | "generation-mismatch"
  | "invalid-transition"
  | "not-found"
  | "not-primary"
  | "unavailable"
  | "network"
  | "unknown";

export class TopologyApiError extends ApiError {
  constructor(
    status: number,
    public readonly kind: TopologyApiErrorKind,
    message: string,
  ) {
    super(status, message);
    this.name = "TopologyApiError";
  }
}

export function promoteTopology(
  expectedGeneration?: number,
  signal?: AbortSignal,
): Promise<TopologyConversionResult> {
  return topologyRequest("/api/topology/promote", expectedGeneration, signal);
}

export function demoteTopology(
  expectedGeneration?: number,
  signal?: AbortSignal,
): Promise<TopologyConversionResult> {
  return topologyRequest("/api/topology/demote", expectedGeneration, signal);
}

async function topologyRequest(
  path: string,
  expectedGeneration: number | undefined,
  signal?: AbortSignal,
): Promise<TopologyConversionResult> {
  let response: Response;
  try {
    response = await fetch(path, {
      method: "POST",
      credentials: "include",
      cache: "no-store",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        confirm: true,
        ...(expectedGeneration === undefined ? {} : { expectedGeneration }),
      }),
      ...(signal ? { signal } : {}),
    });
  } catch {
    throw new TopologyApiError(0, "network", "Topology service could not be reached.");
  }
  if (!response.ok) {
    const body = (await response.json().catch(() => ({}))) as { code?: unknown };
    const status = response.status;
    const kind: TopologyApiErrorKind =
      status === 401
        ? "unauthorized"
        : status === 403
          ? "forbidden"
          : status === 400
            ? "bad-request"
            : status === 409 && body.code === "GENERATION_MISMATCH"
              ? "generation-mismatch"
              : status === 409
                ? "invalid-transition"
                : status >= 500
                  ? "unavailable"
                  : "unknown";
    throw new TopologyApiError(status, kind, `Topology request failed (${status})`);
  }
  return (await response.json().catch(() => ({ ok: true }))) as TopologyConversionResult;
}

async function topologyNodesRequest<T>(
  path: string,
  method: "GET" | "POST",
  signal?: AbortSignal,
): Promise<T> {
  let response: Response;
  try {
    response = await fetch(path, {
      method,
      credentials: "include",
      cache: "no-store",
      ...(method === "POST" ? { headers: { "content-type": "application/json" }, body: "{}" } : {}),
      ...(signal ? { signal } : {}),
    });
  } catch {
    throw new TopologyApiError(0, "network", "Topology service could not be reached.");
  }
  if (!response.ok) {
    const body = (await response.json().catch(() => ({}))) as { code?: unknown };
    const status = response.status;
    const kind: TopologyApiErrorKind =
      status === 401
        ? "unauthorized"
        : status === 403
          ? "forbidden"
          : status === 404
            ? "not-found"
            : status === 409 && body.code === "TOPOLOGY_NOT_PRIMARY"
              ? "not-primary"
              : status === 409
                ? "invalid-transition"
                : status >= 500
                  ? "unavailable"
                  : "unknown";
    throw new TopologyApiError(status, kind, `Topology nodes request failed (${status})`);
  }
  return (await response.json().catch(() => undefined)) as T;
}

/** Lists the nodes enrolled with this primary. Only primary roles may call it. */
export async function listTopologyNodes(signal?: AbortSignal): Promise<TopologyNodeList> {
  const body = await topologyNodesRequest<Partial<TopologyNodeList> | undefined>("/api/topology/nodes", "GET", signal);
  if (!body || typeof body.serverTime !== "string" || !Array.isArray(body.nodes)) {
    throw new TopologyApiError(200, "unknown", "Topology nodes response was malformed.");
  }
  return { serverTime: body.serverTime, nodes: body.nodes };
}

/** Unlinks one enrolled node: its credential is revoked and it is told so on its next pull. */
export function unlinkTopologyNode(nodeId: string, signal?: AbortSignal): Promise<{ nodeId: string; unlinkedAt: string }> {
  return topologyNodesRequest(`/api/topology/nodes/${encodeURIComponent(nodeId)}/unlink`, "POST", signal);
}

export type BackupApiErrorKind =
  | "master_key_mismatch"
  | "passphrase_required"
  | "passphrase_mismatch"
  | "invalid_bundle"
  | "too_large"
  | "unavailable"
  | "unauthorized"
  | "forbidden"
  | "unknown";

export class BackupApiError extends ApiError {
  constructor(
    status: number,
    public readonly kind: BackupApiErrorKind,
    message: string,
  ) {
    super(status, message);
    this.name = "BackupApiError";
  }
}

export type BackupImportTable = {
  name: string;
  rowCount: number;
  action: string;
};

export type BackupImportReport = {
  dryRun: boolean;
  bundleSha256: string;
  formatVersion: number;
  exporterVersion: string;
  createdAt: string;
  installationId: string;
  nodeId: string;
  tables: BackupImportTable[];
  envWouldChange: boolean;
  certsToRestore: string[];
  appliedPostImport: boolean;
  compatibilityConflicts?: string[];
};

export type BackupDownload = { blob: Blob; filename: string };

const BACKUP_FILENAME = "proxycore-backup.zip";
const BACKUP_PASSPHRASE_HEADER = "X-Proxycore-Backup-Passphrase";

// Encodes the passphrase as unpadded base64url over its UTF-8 bytes, as the
// backup endpoints expect. The result must never be logged or put in a URL.
export function encodeBackupPassphrase(passphrase: string): string {
  const bytes = new TextEncoder().encode(passphrase);
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

export async function exportBackup(passphrase?: string): Promise<BackupDownload> {
  const headers: Record<string, string> = {};
  if (passphrase) headers[BACKUP_PASSPHRASE_HEADER] = encodeBackupPassphrase(passphrase);
  const response = await backupFetch("/api/backup/export", {
    method: "POST",
    credentials: "include",
    cache: "no-store",
    headers,
  });
  return {
    blob: await response.blob(),
    filename: backupFilename(response.headers.get("content-disposition")),
  };
}

export async function importBackup(
  file: Blob,
  options: { passphrase?: string; dryRun: boolean },
): Promise<BackupImportReport> {
  const form = new FormData();
  form.append("bundle", file);
  if (options.passphrase) {
    form.append("passphrase", encodeBackupPassphrase(options.passphrase));
  }
  // No explicit content-type: the browser must set the multipart boundary.
  const response = await backupFetch(
    `/api/backup/import?dry_run=${options.dryRun ? "true" : "false"}`,
    { method: "POST", credentials: "include", cache: "no-store", body: form },
  );
  try {
    return (await response.json()) as BackupImportReport;
  } catch {
    throw new BackupApiError(
      response.status,
      "unknown",
      "Backup service returned an invalid response.",
    );
  }
}

async function backupFetch(path: string, init: RequestInit): Promise<Response> {
  let response: Response;
  try {
    response = await fetch(path, init);
  } catch {
    throw new BackupApiError(0, "unknown", "Backup service is unavailable.");
  }
  if (!response.ok) throw await backupResponseError(response);
  return response;
}

async function backupResponseError(response: Response) {
  const body = (await response.json().catch(() => ({}))) as {
    error?: unknown;
    code?: unknown;
  };
  const message =
    typeof body.error === "string" && body.error
      ? body.error
      : `Request failed (${response.status})`;
  const status = response.status;
  let kind: BackupApiErrorKind = "unknown";
  if (status === 409 && body.code === "master_key_mismatch") {
    kind = "master_key_mismatch";
  } else if (status === 401) {
    kind = message === "passphrase mismatch" ? "passphrase_mismatch" : "unauthorized";
  } else if (status === 400) {
    kind = message === "passphrase required" ? "passphrase_required" : "invalid_bundle";
  } else if (status === 413) {
    kind = "too_large";
  } else if (status === 503) {
    kind = "unavailable";
  } else if (status === 403) {
    kind = "forbidden";
  }
  return new BackupApiError(status, kind, message);
}

function backupFilename(disposition: string | null): string {
  const match = disposition?.match(/filename\*?=(?:UTF-8'')?"?([^";]+)"?/i);
  if (!match?.[1]) return BACKUP_FILENAME;
  let name = match[1].trim();
  try {
    name = decodeURIComponent(name);
  } catch {
    // Keep the raw value when it is not percent-encoded.
  }
  const base = name.split(/[\\/]/).pop()?.trim();
  return base && base !== "." && base !== ".." ? base : BACKUP_FILENAME;
}
