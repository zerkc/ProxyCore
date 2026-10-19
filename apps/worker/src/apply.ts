import { randomUUID } from "node:crypto";
import { mkdir, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { transitionJob, type ConfigurationSnapshot } from "@proxycore/domain";
import type {
  JobRecord,
  JobStore,
  RevisionRecord,
  RevisionStore,
} from "@proxycore/db";
import type {
  ControlRequest,
  ControlResponse,
  ControlService,
} from "../../control/src/protocol";

export interface ControlClient {
  execute(request: ControlRequest): Promise<ControlResponse>;
}

export type RenderedCandidate = {
  service: ControlService;
  candidatePath: string;
  checksum: string;
  files?: Record<string, string>;
};

export type CandidateRenderer = (
  snapshot: ConfigurationSnapshot,
  job: JobRecord,
) =>
  | RenderedCandidate
  | RenderedCandidate[]
  | Promise<RenderedCandidate | RenderedCandidate[]>;

const NGINX_RECONCILIATION_CORRELATION = "reconcile-nginx:";

export class ApplyOrchestrator {
  constructor(
    private readonly stores: {
      jobs: JobStore;
      revisions: RevisionStore;
      control: ControlClient;
      candidateRoot?: string;
      now?: () => Date;
      log?: (message: string) => void;
    },
  ) {}

  /**
   * Reconcile one durable applied revision when the Nginx data plane has
   * reverted to the valid baked fallback or lost its promoted configuration.
   * The store atomically suppresses only an active duplicate; terminal repair
   * jobs do not prevent a later drift event from being repaired.
   */
  async reconcileAppliedRevision(
    revision: RevisionRecord,
    render: CandidateRenderer,
  ): Promise<JobRecord | undefined> {
    if (!revision.appliedAt) return undefined;

    const probe = await this.probeNginxActiveConfig(revision);
    if (!nginxNeedsReconciliation(probe)) return undefined;

    const correlationId = `${NGINX_RECONCILIATION_CORRELATION}${revision.id}`;
    const claimedAt = this.now();
    const repairJob = await this.stores.jobs.enqueueIfNotActive({
      revisionId: revision.id,
      target: "nginx",
      status: "validating",
      claimedAt,
      startedAt: claimedAt,
      correlationId,
    });
    if (!repairJob) return undefined;
    return this.apply(repairJob.id, revision.snapshot, render);
  }

  async apply(
    jobId: string,
    snapshot: ConfigurationSnapshot,
    render: CandidateRenderer,
  ): Promise<JobRecord> {
    let job: JobRecord;
    try {
      job = await this.requireJob(jobId);
    } catch (error) {
      this.stores.log?.(
        `worker apply failed job=${safeLogId(jobId)} revision=unavailable step=lookup service=none`,
      );
      throw error;
    }
    try {
      if (job.status === "queued") {
        job = await this.setStatus(job, "validating");
      } else if (job.status !== "validating") {
        throw new Error(`Job ${jobId} is not queued`);
      }
    } catch (error) {
      this.stores.log?.(
        `worker apply failed job=${safeLogId(job.id)} revision=${safeLogId(job.revisionId)} correlation=${safeLogId(job.correlationId)} step=claim service=none`,
      );
      throw error;
    }
    const promoted: RenderedCandidate[] = [];
    let candidates: RenderedCandidate[] = [];
    let step = "render";
    let service = "none";
    const logFailure = (outcome: string) =>
      this.stores.log?.(
        `worker apply ${outcome} job=${safeLogId(job.id)} revision=${safeLogId(job.revisionId)} correlation=${safeLogId(job.correlationId)} step=${step} service=${service}`,
      );

    try {
      const rendered = await render(desiredSnapshotForApply(snapshot), job);
      candidates = Array.isArray(rendered) ? rendered : [rendered];
      if (candidates.length === 0) {
        throw new Error("No candidates were rendered");
      }

      const validationOutput: Record<string, unknown> = {};
      for (const candidate of candidates) {
        service = candidate.service;
        step = "write_candidate";
        await writeCandidate(candidate, this.stores.candidateRoot);
        step = "stage";
        const staged = await this.request(candidate, job, "stage");
        if (!staged.ok) {
          logFailure("failed");
          return await this.fail(
            job,
            staged.error ?? "Candidate staging failed",
            {
              validationOutput: {
                service: candidate.service,
                output: staged.output,
              },
            },
          );
        }
        step = "validate";
        const validation = await this.request(candidate, job, "validate");
        validationOutput[candidate.service] = validation.output;
        if (!validation.ok) {
          logFailure("failed");
          return await this.fail(
            job,
            validation.error ?? "Candidate validation failed",
            {
              validationOutput,
            },
          );
        }
      }

      step = "set_applying";
      job = await this.setStatus(job, "applying", { validationOutput });
      const applyOutput: Record<string, unknown> = {};
      for (const candidate of candidates) {
        service = candidate.service;
        step = "promote";
        const promotedResponse = await this.request(candidate, job, "promote");
        if (!promotedResponse.ok) {
          throw new Error(
            promotedResponse.error ?? "Candidate promotion failed",
          );
        }
        promoted.push(candidate);
        applyOutput[candidate.service] = promotedResponse.output;
      }

      const healthOutput: Record<string, unknown> = {};
      for (const candidate of candidates) {
        service = candidate.service;
        step = "reload";
        const reload = await this.request(candidate, job, "reload");
        if (!reload.ok) {
          throw new Error(reload.error ?? "Service reload failed");
        }
        step = "health";
        const health = await this.request(candidate, job, "health");
        healthOutput[candidate.service] = health.output;
        if (!health.ok || hasNginxDrift(health)) {
          logFailure("failed");
          const rollbackSucceeded = await this.rollback(promoted, job);
          if (rollbackSucceeded) {
            return await this.setStatus(job, "rolled-back", {
              healthOutput,
              errorMessage:
                health.error ?? "Post-reload health failed; rolled back",
              finishedAt: this.now(),
            });
          }
          return await this.fail(
            job,
            `${health.error ?? "Post-reload health failed"}; rollback failed`,
            { healthOutput },
          );
        }
      }

      step = "mark_applied";
      await this.stores.revisions.markApplied(job.revisionId, this.now());
      return await this.setStatus(job, "applied", {
        applyOutput,
        healthOutput,
        finishedAt: this.now(),
      });
    } catch (error) {
      logFailure("failed");
      if (promoted.length > 0 && (await this.rollback(promoted, job))) {
        return await this.setStatus(job, "rolled-back", {
          errorMessage: errorMessage(error),
          finishedAt: this.now(),
        });
      }
      return await this.fail(job, errorMessage(error));
    }
  }

  private async probeNginxActiveConfig(
    revision: RevisionRecord,
  ): Promise<ControlResponse> {
    return this.stores.control.execute({
      requestId: randomUUID(),
      operation: "health",
      service: "nginx",
      revisionId: revision.id,
      checksum: revision.checksum,
      candidatePath: join(
        this.stores.candidateRoot ?? "/var/lib/proxycore/candidates",
        revision.id,
        "nginx",
      ),
    });
  }

  private async requireJob(jobId: string): Promise<JobRecord> {
    const job = await this.stores.jobs.get(jobId);
    if (!job) throw new Error(`Job not found: ${jobId}`);
    return job;
  }

  private async setStatus(
    job: JobRecord,
    status: JobRecord["status"],
    patch: Partial<JobRecord> = {},
  ): Promise<JobRecord> {
    const next = transitionJob(job.status, status);
    return this.stores.jobs.update(job.id, { ...patch, status: next });
  }

  private async fail(
    job: JobRecord,
    message: string,
    patch: Partial<JobRecord> = {},
  ): Promise<JobRecord> {
    return this.setStatus(job, "failed", {
      ...patch,
      errorMessage: message,
      finishedAt: this.now(),
    });
  }

  private async rollback(
    candidates: RenderedCandidate[],
    job: JobRecord,
  ): Promise<boolean> {
    let succeeded = true;
    for (const candidate of [...candidates].reverse()) {
      const response = await this.request(candidate, job, "rollback").catch(
        () => ({
          ok: false,
          requestId: "",
          operation: "rollback" as const,
        }),
      );
      if (!response.ok) {
        this.stores.log?.(
          `worker rollback failed job=${safeLogId(job.id)} revision=${safeLogId(job.revisionId)} correlation=${safeLogId(job.correlationId)} service=${candidate.service}`,
        );
      }
      succeeded = response.ok && succeeded;
    }
    return succeeded;
  }

  private async request(
    candidate: RenderedCandidate,
    job: JobRecord,
    operation: ControlRequest["operation"],
  ): Promise<ControlResponse> {
    return this.stores.control.execute({
      requestId: randomUUID(),
      operation,
      service: candidate.service,
      revisionId: job.revisionId,
      checksum: candidate.checksum,
      candidatePath: candidate.candidatePath,
    });
  }

  private now(): Date {
    return this.stores.now?.() ?? new Date();
  }
}

function safeLogId(value: string): string {
  return /^[A-Za-z0-9_-]{1,80}$/.test(value) ? value : "[redacted]";
}

// desiredSnapshotForApply unwraps the replication envelope. A node renders a
// near-exact copy of the primary with one exception: its own ingress. The
// envelope's nodeLocal.ingress is authoritative on the receiving installation
// (the API importer rewrites it with the node's own address), so it replaces
// settings.ingress here, and proxied domains answer THIS installation's IP.
// The substitution happens at render time, not in the stored envelope, so the
// primary's contentHash and the revision checksum stay verifiable.
function desiredSnapshotForApply(
  snapshot: ConfigurationSnapshot,
): ConfigurationSnapshot {
  if (!snapshot || typeof snapshot !== "object") return snapshot;
  const replicated = (snapshot as { replicated?: unknown }).replicated;
  if (!replicated || typeof replicated !== "object") return snapshot;
  const configuration = (replicated as { configuration?: unknown })
    .configuration;
  if (!configuration || typeof configuration !== "object") return snapshot;
  const desired = configuration as ConfigurationSnapshot;
  const local = (snapshot as { nodeLocal?: { ingress?: unknown } }).nodeLocal
    ?.ingress as { ipv4?: unknown; ipv6?: unknown } | undefined;
  const ipv4 =
    typeof local?.ipv4 === "string" && local.ipv4 ? local.ipv4 : undefined;
  const ipv6 =
    typeof local?.ipv6 === "string" && local.ipv6 ? local.ipv6 : undefined;
  const settings = (desired as { settings?: unknown }).settings;
  if ((!ipv4 && !ipv6) || !settings || typeof settings !== "object") {
    return desired;
  }
  return {
    ...desired,
    settings: {
      ...(settings as object),
      ingress: {
        ...(ipv4 ? { ipv4 } : {}),
        ...(ipv6 ? { ipv6 } : {}),
      },
    },
  } as ConfigurationSnapshot;
}

function hasNginxDrift(response: ControlResponse): boolean {
  if (!response.output || typeof response.output !== "object") return false;
  return (response.output as { status?: unknown }).status === "drift";
}

function nginxNeedsReconciliation(response: ControlResponse): boolean {
  return response.ok && hasNginxDrift(response);
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : "Apply failed";
}

async function writeCandidate(
  candidate: RenderedCandidate,
  candidateRoot = "/var/lib/proxycore/candidates",
): Promise<void> {
  if (!candidate.files) return;
  const normalizedRoot = candidateRoot.replace(/\/+$/, "");
  if (
    !candidate.candidatePath.startsWith(`${normalizedRoot}/`) ||
    candidate.candidatePath.includes("..")
  ) {
    throw new Error("Candidate path is outside the worker candidate root");
  }
  await mkdir(candidate.candidatePath, { recursive: true });
  for (const [relativePath, contents] of Object.entries(candidate.files)) {
    if (
      relativePath.startsWith("/") ||
      relativePath.includes("..") ||
      relativePath.includes("\0")
    ) {
      throw new Error("Candidate file path is invalid");
    }
    const target = join(candidate.candidatePath, relativePath);
    await mkdir(join(target, ".."), { recursive: true });
    // Nginx workers must read auth/cert candidate files after reload.
    await writeFile(target, contents, { encoding: "utf8", mode: 0o644 });
  }
}
