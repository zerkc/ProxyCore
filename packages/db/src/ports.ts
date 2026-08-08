import { randomUUID } from "node:crypto";
import {
  checksumSnapshot,
  createSnapshot,
  type ConfigurationSnapshot,
  type JobStatus,
} from "@proxycore/domain";

export type RevisionRecord = {
  id: string;
  revisionNumber: number;
  checksum: string;
  snapshot: ConfigurationSnapshot;
  actorUserId?: string;
  createdAt: Date;
  appliedAt?: Date;
};

export interface RevisionStore {
  create(snapshot: ConfigurationSnapshot, actorUserId?: string): Promise<RevisionRecord>;
  get(id: string): Promise<RevisionRecord | undefined>;
  latest(): Promise<RevisionRecord | undefined>;
  markApplied(id: string, appliedAt?: Date): Promise<RevisionRecord>;
}

export type JobTarget = "coredns" | "nginx" | "combined" | "certificate";

export type JobRecord = {
  id: string;
  revisionId: string;
  actorUserId?: string;
  target: JobTarget;
  status: JobStatus;
  correlationId: string;
  createdAt: Date;
  claimedAt?: Date;
  startedAt?: Date;
  finishedAt?: Date;
  validationOutput?: unknown;
  applyOutput?: unknown;
  healthOutput?: unknown;
  errorMessage?: string;
};

export interface JobStore {
  enqueue(
    job: Omit<JobRecord, "id" | "status" | "createdAt"> & { status?: JobStatus },
  ): Promise<JobRecord>;
  get(id: string): Promise<JobRecord | undefined>;
  claimNext(target?: JobTarget): Promise<JobRecord | undefined>;
  recoverStale(leaseMs: number, now?: Date): Promise<number>;
  update(id: string, patch: Partial<JobRecord>): Promise<JobRecord>;
  list(): Promise<JobRecord[]>;
}

export class InMemoryRevisionStore implements RevisionStore {
  private readonly records = new Map<string, RevisionRecord>();
  private nextNumber = 1;

  async create(snapshot: ConfigurationSnapshot, actorUserId?: string): Promise<RevisionRecord> {
    const normalized = createSnapshot(snapshot);
    const record: RevisionRecord = {
      id: randomUUID(),
      revisionNumber: this.nextNumber++,
      checksum: checksumSnapshot(normalized),
      snapshot: normalized,
      actorUserId,
      createdAt: new Date(),
    };
    this.records.set(record.id, record);
    return record;
  }

  async get(id: string): Promise<RevisionRecord | undefined> {
    return this.records.get(id);
  }

  async latest(): Promise<RevisionRecord | undefined> {
    return [...this.records.values()].at(-1);
  }

  async markApplied(id: string, appliedAt = new Date()): Promise<RevisionRecord> {
    const record = this.records.get(id);
    if (!record) {
      throw new Error(`Revision not found: ${id}`);
    }
    const applied = { ...record, appliedAt };
    this.records.set(id, applied);
    return applied;
  }
}

export class InMemoryJobStore implements JobStore {
  private readonly records = new Map<string, JobRecord>();
  private readonly activeTargets = new Set<JobRecord["target"]>();

  async enqueue(
    job: Omit<JobRecord, "id" | "status" | "createdAt"> & { status?: JobStatus },
  ): Promise<JobRecord> {
    const record: JobRecord = {
      ...job,
      id: randomUUID(),
      status: job.status ?? "queued",
      createdAt: new Date(),
    };
    this.records.set(record.id, record);
    return record;
  }

  async get(id: string): Promise<JobRecord | undefined> {
    return this.records.get(id);
  }

  async claimNext(target?: JobTarget): Promise<JobRecord | undefined> {
    if (target && this.activeTargets.has(target)) {
      return undefined;
    }
    const next = [...this.records.values()].find(
      (job) =>
        (target === undefined || job.target === target) &&
        job.status === "queued" &&
        !this.activeTargets.has(job.target),
    );
    if (!next) {
      return undefined;
    }
    this.activeTargets.add(next.target);
    return this.update(next.id, {
      status: "validating",
      claimedAt: new Date(),
      startedAt: new Date(),
    });
  }

  async recoverStale(leaseMs: number, now = new Date()): Promise<number> {
    let recovered = 0;
    for (const job of this.records.values()) {
      if (
        (job.status === "validating" || job.status === "applying") &&
        job.claimedAt &&
        now.getTime() - job.claimedAt.getTime() > leaseMs
      ) {
        this.records.set(job.id, {
          ...job,
          status: "queued",
          claimedAt: undefined,
          startedAt: undefined,
        });
        this.activeTargets.delete(job.target);
        recovered += 1;
      }
    }
    return recovered;
  }

  async update(id: string, patch: Partial<JobRecord>): Promise<JobRecord> {
    const current = this.records.get(id);
    if (!current) {
      throw new Error(`Job not found: ${id}`);
    }
    const updated = { ...current, ...patch };
    this.records.set(id, updated);
    if (
      updated.status === "applied" ||
      updated.status === "failed" ||
      updated.status === "rolled-back"
    ) {
      this.activeTargets.delete(updated.target);
    }
    return updated;
  }

  async list(): Promise<JobRecord[]> {
    return [...this.records.values()];
  }
}
