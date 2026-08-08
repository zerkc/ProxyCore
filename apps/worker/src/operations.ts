import { redactSecrets } from "@proxycore/crypto";

export type RetentionArtifact = {
  id: string;
  kind: string;
  sizeBytes: number;
  createdAt: Date;
  current?: boolean;
  activeCertificate?: boolean;
};

export type RetentionPolicy = {
  maxAgeDays: number;
  maxSizeBytes: number;
};

export function selectRetentionDeletions(
  artifacts: RetentionArtifact[],
  policy: RetentionPolicy,
  now: Date,
): string[] {
  const protectedIds = new Set(
    artifacts
      .filter((artifact) => artifact.current || artifact.activeCertificate)
      .map((artifact) => artifact.id),
  );
  const candidates = artifacts
    .filter((artifact) => !protectedIds.has(artifact.id))
    .sort((left, right) => left.createdAt.getTime() - right.createdAt.getTime());
  const deleted = new Set<string>();
  const ageCutoff = now.getTime() - policy.maxAgeDays * 24 * 60 * 60 * 1_000;

  for (const artifact of candidates) {
    if (artifact.createdAt.getTime() < ageCutoff) {
      deleted.add(artifact.id);
    }
  }

  let remainingSize = artifacts.reduce((sum, artifact) => sum + artifact.sizeBytes, 0);
  for (const artifact of candidates) {
    if (remainingSize <= policy.maxSizeBytes) break;
    if (!deleted.has(artifact.id)) {
      deleted.add(artifact.id);
    }
    remainingSize -= artifact.sizeBytes;
  }
  return candidates.filter((artifact) => deleted.has(artifact.id)).map((artifact) => artifact.id);
}

export type HealthStatus = "healthy" | "degraded" | "unhealthy" | "unknown";

export type HealthObservation = {
  component: "app" | "worker" | "coredns" | "nginx" | "upstream" | "dns-forwarder";
  status: HealthStatus;
  details: Record<string, unknown>;
};

export type HealthSummary = {
  overall: HealthStatus;
  components: Partial<Record<HealthObservation["component"], HealthObservation>>;
};

export function summarizeHealth(observations: HealthObservation[]): HealthSummary {
  const components = Object.fromEntries(
    observations.map((observation) => [observation.component, observation]),
  ) as HealthSummary["components"];
  const statuses = observations.map((observation) => observation.status);
  const overall: HealthStatus =
    statuses.includes("unhealthy")
      ? "unhealthy"
      : statuses.includes("degraded") || statuses.includes("unknown")
        ? "degraded"
        : statuses.length > 0
          ? "healthy"
          : "unknown";
  return { overall, components };
}

export function serializeStructuredLog(
  level: "info" | "warn" | "error",
  event: string,
  fields: Record<string, unknown> = {},
): string {
  return JSON.stringify({
    timestamp: new Date().toISOString(),
    level,
    event,
    ...(redactSecrets(fields) as Record<string, unknown>),
  });
}
