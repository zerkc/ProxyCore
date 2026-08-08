import type { PublicUser, Role } from "@proxycore/domain";
import { getWebContext } from "./context";

export class HttpError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
  }
}

export async function requireUser(
  request: Request,
  roles?: Role[],
): Promise<{ user: PublicUser; token: string }> {
  const token = sessionToken(request);
  if (!token) throw new HttpError(401, "Authentication required");
  let user: PublicUser;
  try {
    user = await getWebContext().auth.authenticate(token);
  } catch {
    throw new HttpError(401, "Authentication required");
  }
  if (roles && !roles.includes(user.role)) {
    throw new HttpError(403, "Permission denied");
  }
  return { user, token };
}

export function sessionToken(request: Request): string | undefined {
  const authorization = request.headers.get("authorization");
  if (authorization?.startsWith("Bearer ")) {
    return authorization.slice("Bearer ".length).trim();
  }
  const cookieHeader = request.headers.get("cookie") ?? "";
  const cookieName = getWebContext().config.sessionCookieName;
  return cookieHeader
    .split(";")
    .map((part) => part.trim())
    .find((part) => part.startsWith(`${cookieName}=`))
    ?.slice(cookieName.length + 1);
}

export async function readJson(request: Request): Promise<Record<string, unknown>> {
  const body: unknown = await request.json().catch(() => undefined);
  if (!body || typeof body !== "object" || Array.isArray(body)) {
    throw new HttpError(400, "JSON object body is required");
  }
  return body as Record<string, unknown>;
}

export function apiError(error: unknown): Response {
  if (error instanceof HttpError) {
    return Response.json({ error: error.message }, { status: error.status });
  }
  return Response.json(
    { error: error instanceof Error ? error.message : "Request failed" },
    { status: 400 },
  );
}

export function sessionCookie(token: string, expiresAt: Date): string {
  const config = getWebContext().config;
  const secure = config.nodeEnv === "production" ? "; Secure" : "";
  return `${config.sessionCookieName}=${token}; Path=/; HttpOnly; SameSite=Lax; Expires=${expiresAt.toUTCString()}${secure}`;
}

export function clearSessionCookie(): string {
  const config = getWebContext().config;
  return `${config.sessionCookieName}=; Path=/; HttpOnly; SameSite=Lax; Max-Age=0`;
}
