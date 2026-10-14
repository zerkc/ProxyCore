import { ApiError } from "../api";
import type { TopologySyncStatus } from "./types";

export type TopologySyncErrorKind = "unauthorized" | "forbidden" | "not-available" | "sync-failed" | "network" | "unknown";

export class TopologySyncError extends ApiError {
  constructor(status: number, public readonly kind: TopologySyncErrorKind) {
    super(status, `Topology sync request failed (${status})`);
    this.name = "TopologySyncError";
  }
}

async function syncRequest(path: string, method: "GET" | "POST", signal?: AbortSignal): Promise<Response> {
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
    throw new TopologySyncError(0, "network");
  }
  if (!response || typeof response.ok !== "boolean") throw new TopologySyncError(0, "network");
  if (!response.ok) {
    const status = response.status;
    const kind: TopologySyncErrorKind =
      status === 401 ? "unauthorized" : status === 403 ? "forbidden" : status === 409 ? "not-available" : status === 502 ? "sync-failed" : "unknown";
    throw new TopologySyncError(status, kind);
  }
  return response;
}

export async function getTopologySync(signal?: AbortSignal): Promise<TopologySyncStatus> {
  const response = await syncRequest("/api/topology/sync", "GET", signal);
  const body = (await response.json().catch(() => undefined)) as Partial<TopologySyncStatus> | undefined;
  if (!body || typeof body !== "object" || typeof body.role !== "string") throw new TopologySyncError(response.status, "unknown");
  return {
    role: body.role,
    bound: body.bound === true,
    outdated: body.outdated === true,
    consecutiveFailures: typeof body.consecutiveFailures === "number" ? body.consecutiveFailures : 0,
    acknowledged: body.acknowledged === true,
    unlinked: body.unlinked === true,
    ...(body.unlinkedAt ? { unlinkedAt: body.unlinkedAt } : {}),
    ...(body.appliedHash ? { appliedHash: body.appliedHash } : {}),
    ...(body.primaryHash ? { primaryHash: body.primaryHash } : {}),
    ...(body.lastSyncAt ? { lastSyncAt: body.lastSyncAt } : {}),
    ...(body.lastAttemptAt ? { lastAttemptAt: body.lastAttemptAt } : {}),
    ...(body.lastError ? { lastError: body.lastError } : {}),
    ...(body.nextAttemptAt ? { nextAttemptAt: body.nextAttemptAt } : {}),
  };
}

export async function retryTopologySync(signal?: AbortSignal): Promise<void> {
  await syncRequest("/api/topology/sync/retry", "POST", signal);
}

export function shortHash(value?: string) {
  return value ? value.slice(0, 12) : "—";
}

/** Returns the warning text when promoting this node would not be fully verified, otherwise undefined. */
export function promoteSyncWarning(status: TopologySyncStatus | "unverified"): string | undefined {
  if (status === "unverified") return "The sync state of this NODE could not be verified. It may be missing recent PRIMARY changes.";
  if (!status.appliedHash) return "This NODE has never synced from the PRIMARY. Promoting it now would publish its own, possibly empty, configuration.";
  if (status.outdated) return "This NODE is outdated: the PRIMARY has published newer configuration that this NODE has not applied yet.";
  return undefined;
}
