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
  create(snapshot: ConfigurationSnapshot, actorUserId?: string): RevisionRecord;
  get(id: string): RevisionRecord | undefined;
  latest(): RevisionRecord | undefined;
  markApplied(id: string, appliedAt?: Date): RevisionRecord;
}

export type JobRecord = {
  id: string;
  revisionId: string;
  target: "coredns" | "nginx" | "combined" | "certificate";
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
  ): JobRecord;
  get(id: string): JobRecord | undefined;
  claimNext(target: JobRecord["target"]): JobRecord | undefined;
  update(id: string, patch: Partial<JobRecord>): JobRecord;
  list(): JobRecord[];
}

export class InMemoryRevisionStore implements RevisionStore {
  private readonly records = new Map<string, RevisionRecord>();
  private nextNumber = 1;

  create(snapshot: ConfigurationSnapshot, actorUserId?: string): RevisionRecord {
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

  get(id: string): RevisionRecord | undefined {
    return this.records.get(id);
  }

  latest(): RevisionRecord | undefined {
    return [...this.records.values()].at(-1);
  }

  markApplied(id: string, appliedAt = new Date()): RevisionRecord {
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

  enqueue(
    job: Omit<JobRecord, "id" | "status" | "createdAt"> & { status?: JobStatus },
  ): JobRecord {
    const record: JobRecord = {
      ...job,
      id: randomUUID(),
      status: job.status ?? "queued",
      createdAt: new Date(),
    };
    this.records.set(record.id, record);
    return record;
  }

  get(id: string): JobRecord | undefined {
    return this.records.get(id);
  }

  claimNext(target: JobRecord["target"]): JobRecord | undefined {
    if (this.activeTargets.has(target)) {
      return undefined;
    }
    const next = [...this.records.values()].find(
      (job) => job.target === target && job.status === "queued",
    );
    if (!next) {
      return undefined;
    }
    this.activeTargets.add(target);
    return this.update(next.id, {
      status: "validating",
      claimedAt: new Date(),
      startedAt: new Date(),
    });
  }

  update(id: string, patch: Partial<JobRecord>): JobRecord {
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

  list(): JobRecord[] {
    return [...this.records.values()];
  }
}
