import type { EnrollmentHostnameConfig } from "./dashboard/types";

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
  const body = (await response.json().catch(() => ({}))) as {
    error?: string;
  } & T;
  if (!response.ok) {
    throw new ApiError(
      response.status,
      body.error ?? `Request failed (${response.status})`,
    );
  }
  return body;
}

export function getEnrollmentHostnames() {
  return api<EnrollmentHostnameConfig>(
    "/api/settings/enrollment-hostnames",
    { cache: "no-store" },
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
