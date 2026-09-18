import type {
  EnrollmentHostnameConfig,
  EnrollmentTrust,
  EnrollmentTrustDownload,
} from "./dashboard/types";

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
