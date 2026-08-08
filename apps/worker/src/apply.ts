import { randomUUID } from "node:crypto";
import { mkdir, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { transitionJob, type ConfigurationSnapshot } from "@proxycore/domain";
import type { JobRecord, JobStore, RevisionStore } from "@proxycore/db";
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

export class ApplyOrchestrator {
  constructor(
    private readonly stores: {
      jobs: JobStore;
      revisions: RevisionStore;
      control: ControlClient;
      candidateRoot?: string;
      now?: () => Date;
    },
  ) {}

  async apply(
    jobId: string,
    snapshot: ConfigurationSnapshot,
    render: CandidateRenderer,
  ): Promise<JobRecord> {
    let job = await this.requireJob(jobId);
    if (job.status === "queued") {
      job = await this.setStatus(job, "validating");
    } else if (job.status !== "validating") {
      throw new Error(`Job ${jobId} is not queued`);
    }
    const promoted: RenderedCandidate[] = [];
    let candidates: RenderedCandidate[] = [];

    try {
      const rendered = await render(snapshot, job);
      candidates = Array.isArray(rendered) ? rendered : [rendered];
      if (candidates.length === 0) {
        throw new Error("No candidates were rendered");
      }

      const validationOutput: Record<string, unknown> = {};
      for (const candidate of candidates) {
        await writeCandidate(candidate, this.stores.candidateRoot);
        const staged = await this.request(candidate, job, "stage");
        if (!staged.ok) {
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
        const validation = await this.request(candidate, job, "validate");
        validationOutput[candidate.service] = validation.output;
        if (!validation.ok) {
          return await this.fail(
            job,
            validation.error ?? "Candidate validation failed",
            {
              validationOutput,
            },
          );
        }
      }

      job = await this.setStatus(job, "applying", { validationOutput });
      const applyOutput: Record<string, unknown> = {};
      for (const candidate of candidates) {
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
        const reload = await this.request(candidate, job, "reload");
        if (!reload.ok) {
          throw new Error(reload.error ?? "Service reload failed");
        }
        const health = await this.request(candidate, job, "health");
        healthOutput[candidate.service] = health.output;
        if (!health.ok) {
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

      await this.stores.revisions.markApplied(job.revisionId, this.now());
      return await this.setStatus(job, "applied", {
        applyOutput,
        healthOutput,
        finishedAt: this.now(),
      });
    } catch (error) {
      if (promoted.length > 0 && (await this.rollback(promoted, job))) {
        return await this.setStatus(job, "rolled-back", {
          errorMessage: errorMessage(error),
          finishedAt: this.now(),
        });
      }
      return await this.fail(job, errorMessage(error));
    }
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

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : "Apply failed";
}

async function writeCandidate(
  candidate: RenderedCandidate,
  candidateRoot = "/var/lib/proxycore",
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
