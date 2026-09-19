import type {
  EnrollmentHostnameConfig,
  EnrollmentTrust,
  EnrollmentTrustDownload,
} from "./dashboard/types";
import type {
  ConfirmResult,
  DraftPreview,
  EnrollmentTokenCreation,
  EnrollmentTokenSummary,
  RecoverableState,
} from "./types";

export type {
  ConfirmResult,
  DraftPreview,
  EnrollmentTokenCreation,
  EnrollmentTokenSummary,
  RecoverableState,
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
  | "payload-too-large";

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
  | "list-tokens"
  | "revoke-token"
  | "draft"
  | "preview"
  | "confirm"
  | "recover";

export async function createEnrollmentToken(
  signal?: AbortSignal,
): Promise<EnrollmentTokenCreation> {
  const body = await enrollmentRequest<unknown>(
    "create-token",
    "/api/enrollment/tokens",
    { method: "POST", body: "{}" },
    signal,
  );
  return normalizeTokenCreation(body);
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
  primaryURL: string,
  signal?: AbortSignal,
): Promise<DraftPreview> {
  return enrollmentRequest<DraftPreview>(
    "draft",
    "/api/topology/enrollment/draft",
    {
      method: "POST",
      body: JSON.stringify({ token, primaryURL }),
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
    try {
      await readBoundedText(response);
    } catch {
      // Error response contents are intentionally discarded, including secrets.
    }
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
