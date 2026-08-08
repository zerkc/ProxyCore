import { randomUUID } from "node:crypto";
import {
  transitionJob,
  type ConfigurationSnapshot,
} from "@proxycore/domain";
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
};

export type CandidateRenderer = (
  snapshot: ConfigurationSnapshot,
  job: JobRecord,
) => RenderedCandidate;

export class ApplyOrchestrator {
  constructor(
    private readonly stores: {
      jobs: JobStore;
      revisions: RevisionStore;
      control: ControlClient;
      now?: () => Date;
    },
  ) {}

  async apply(
    jobId: string,
    snapshot: ConfigurationSnapshot,
    render: CandidateRenderer,
  ): Promise<JobRecord> {
    let job = this.requireJob(jobId);
    if (job.status !== "queued") {
      throw new Error(`Job ${jobId} is not queued`);
    }
    let promoted = false;
    let candidate: RenderedCandidate | undefined;

    try {
      job = this.setStatus(job, "validating");
      candidate = render(snapshot, job);
      const staged = await this.request(candidate, job, "stage");
      if (!staged.ok) {
        return this.fail(job, staged.error ?? "Candidate staging failed", {
          validationOutput: staged.output,
        });
      }
      const validation = await this.request(candidate, job, "validate");
      if (!validation.ok) {
        return this.fail(job, validation.error ?? "Candidate validation failed", {
          validationOutput: validation.output,
        });
      }

      job = this.setStatus(job, "applying", { validationOutput: validation.output });
      const promotedResponse = await this.request(candidate, job, "promote");
      if (!promotedResponse.ok) {
        return this.fail(job, promotedResponse.error ?? "Candidate promotion failed", {
          applyOutput: promotedResponse.output,
        });
      }
      promoted = true;

      const reload = await this.request(candidate, job, "reload");
      if (!reload.ok) {
        throw new Error(reload.error ?? "Service reload failed");
      }
      const health = await this.request(candidate, job, "health");
      if (!health.ok) {
        const rollback = await this.request(candidate, job, "rollback");
        if (rollback.ok) {
          return this.setStatus(job, "rolled-back", {
            healthOutput: health.output,
            errorMessage: health.error ?? "Post-reload health failed; rolled back",
            finishedAt: this.now(),
          });
        }
        return this.fail(
          job,
          `${health.error ?? "Post-reload health failed"}; rollback failed`,
          { healthOutput: health.output },
        );
      }

      this.stores.revisions.markApplied(job.revisionId, this.now());
      return this.setStatus(job, "applied", {
        healthOutput: health.output,
        finishedAt: this.now(),
      });
    } catch (error) {
      if (promoted && candidate) {
        const rollback = await this.request(candidate, job, "rollback").catch(() => ({
          ok: false,
          requestId: "",
          operation: "rollback" as const,
        }));
        if (rollback.ok) {
          return this.setStatus(job, "rolled-back", {
            errorMessage: errorMessage(error),
            finishedAt: this.now(),
          });
        }
      }
      return this.fail(job, errorMessage(error));
    }
  }

  private requireJob(jobId: string): JobRecord {
    const job = this.stores.jobs.get(jobId);
    if (!job) throw new Error(`Job not found: ${jobId}`);
    return job;
  }

  private setStatus(
    job: JobRecord,
    status: JobRecord["status"],
    patch: Partial<JobRecord> = {},
  ): JobRecord {
    const next = transitionJob(job.status, status);
    return this.stores.jobs.update(job.id, { ...patch, status: next });
  }

  private fail(job: JobRecord, message: string, patch: Partial<JobRecord> = {}): JobRecord {
    return this.setStatus(job, "failed", {
      ...patch,
      errorMessage: message,
      finishedAt: this.now(),
    });
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
