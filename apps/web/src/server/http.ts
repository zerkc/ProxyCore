import { isIP } from "node:net";
import type { PublicUser, Role } from "@proxycore/domain";
import type { IngressAddresses } from "@proxycore/domain";
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
  const context = getWebContext();
  const token = sessionToken(request);
  if (!token) throw new HttpError(401, "Authentication required");
  let user: PublicUser;
  try {
    user = await context.auth.authenticate(token);
  } catch {
    throw new HttpError(401, "Authentication required");
  }
  if (roles && !roles.includes(user.role)) {
    throw new HttpError(403, "Permission denied");
  }
  const requestIngress = inferRequestIngress(request);
  const ingressInitialized = await context.configuration.initializeIngress({
    ipv4: context.defaultIngress.ipv4 ?? requestIngress.ipv4,
    ipv6: context.defaultIngress.ipv6 ?? requestIngress.ipv6,
  });
  if (ingressInitialized && (await context.configuration.getSettings()).defaultPool) {
    await context.configuration.createApplyJob(user.id);
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

function inferRequestIngress(request: Request): IngressAddresses {
  const rawHost = request.headers.get("host")?.trim();
  if (!rawHost) return {};

  let hostname = rawHost;
  if (rawHost.startsWith("[")) {
    const end = rawHost.indexOf("]");
    hostname = end > 0 ? rawHost.slice(1, end) : rawHost;
  } else if (rawHost.indexOf(":") === rawHost.lastIndexOf(":")) {
    hostname = rawHost.split(":")[0] ?? rawHost;
  }

  if (
    isIP(hostname) === 4 &&
    !isLoopbackIpv4(hostname) &&
    hostname !== "0.0.0.0" &&
    isPrivateIpv4(hostname)
  ) {
    return { ipv4: hostname };
  }
  if (isIP(hostname) === 6 && hostname !== "::" && hostname !== "::1" && isLanIpv6(hostname)) {
    return { ipv6: hostname };
  }
  return {};
}

function isLoopbackIpv4(address: string): boolean {
  return address === "127.0.0.1" || address.startsWith("127.");
}

function isPrivateIpv4(address: string): boolean {
  const octets = address.split(".").map(Number);
  return (
    octets[0] === 10 ||
    (octets[0] === 172 && octets[1] >= 16 && octets[1] <= 31) ||
    (octets[0] === 192 && octets[1] === 168)
  );
}

function isLanIpv6(address: string): boolean {
  return address.toLowerCase().startsWith("fc") || address.toLowerCase().startsWith("fd");
}
