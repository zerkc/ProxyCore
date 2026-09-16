import { randomUUID } from "node:crypto";
import {
  checksumSnapshot,
  createSnapshot,
  type ConfigurationSnapshot,
  type JobStatus,
} from "@proxycore/domain";

export type PersistenceSource = "ordinary" | "import" | "sync";
export type EnrollmentAttemptState =
  | "draft"
  | "verified"
  | "confirmed"
  | "exchanged"
  | "archived"
  | "initial-apply-pending"
  | "committed"
  | "cancelled"
  | "recoverable"
  | "failed";
export type SyncTrigger = "enrollment" | "periodic" | "manual" | "restart";
export type SyncAttemptStatus =
  "queued" | "running" | "current" | "applied" | "failed" | "pending-ack";
export type AppliedSnapshotStatus =
  "pending" | "applied" | "rejected" | "rolled-back" | "archived";

export type NodeStateRecord = {
  id: string;
  enrolledAt?: Date;
  enrollmentPrimaryId?: string;
  lastSeenAt?: Date;
  lastAppliedSnapshotId?: string;
  enrollmentAttemptId?: string;
  primaryUrl?: string;
  primaryInstallationId?: string;
  primaryTlsSpkiSha256?: string;
  credentialId?: string;
  syncEnabled: boolean;
  lastAttemptAt?: Date;
  lastSuccessAt?: Date;
  consecutiveFailures: number;
  nextAttemptAt?: Date;
  lastErrorCode?: string;
  updatedAt: Date;
};

export type EnrollmentAttemptRecord = {
  id: string;
  state: EnrollmentAttemptState;
  primaryUrl: string;
  expectedPrimaryId?: string;
  verifiedPrimaryId?: string;
  verifiedPrimaryNodeId?: string;
  verifiedLeadershipGeneration?: number;
  verifiedPrimaryTlsSpkiSha256?: string;
  verifiedPrimaryCaFingerprint?: string;
  previewDigest?: string;
  localNodeIp: string;
  archiveId?: string;
  ephemeralPrivateKeyWrapped: string;
  bootstrapPayload?: Uint8Array;
  nodeCredentialSecretId?: string;
  clusterKeyId?: string;
  initialSnapshotHash?: string;
  initialSnapshotRevisionId?: string;
  initialApplyJobId?: string;
  failureCode?: string;
  confirmedAt?: Date;
  createdAt: Date;
  updatedAt: Date;
};

export type SyncAttemptRecord = {
  id: string;
  nodeId: string;
  trigger: SyncTrigger;
  status: SyncAttemptStatus;
  sourcePrimaryId?: string;
  leadershipGeneration?: number;
  snapshotVersion?: number;
  replicationVersion?: number;
  contentHash?: string;
  revisionId?: string;
  applyJobId?: string;
  resultCode?: string;
  startedAt?: Date;
  finishedAt?: Date;
  createdAt: Date;
  updatedAt: Date;
};

export type AppliedSnapshotRecord = {
  id: string;
  sourcePrimaryId: string;
  leadershipGeneration: number;
  snapshotVersion: number;
  replicationVersion: number;
  contentHash: string;
  revisionId?: string;
  status: AppliedSnapshotStatus;
  applyJobId?: string;
  failureCode?: string;
  appliedAt: Date;
  discardedAt?: Date;
};

export type SnapshotAcknowledgement = {
  nodeId: string;
  contentHash: string;
  snapshotVersion: number;
  replicationVersion: number;
  revisionId: string;
  leadershipGeneration: number;
  appliedAt: Date;
  receivedAt: Date;
};

export interface ContinuityTransactionPort {
  getNodeState(): Promise<NodeStateRecord | undefined>;
  saveNodeState(state: NodeStateRecord): Promise<void>;
  createEnrollmentAttempt(attempt: EnrollmentAttemptRecord): Promise<void>;
  getEnrollmentAttempt(
    id: string,
  ): Promise<EnrollmentAttemptRecord | undefined>;
  recordSyncAttempt(attempt: SyncAttemptRecord): Promise<void>;
  recordAppliedSnapshot(snapshot: AppliedSnapshotRecord): Promise<void>;
  recordSnapshotAcknowledgement(ack: SnapshotAcknowledgement): Promise<void>;
  getSnapshotAcknowledgement(
    nodeId: string,
    contentHash: string,
  ): Promise<SnapshotAcknowledgement | undefined>;
}

export interface ContinuityPersistencePort {
  withTransaction<T>(
    work: (tx: ContinuityTransactionPort) => Promise<T>,
  ): Promise<T>;
}

export class InMemoryContinuityPersistence implements ContinuityPersistencePort {
  private readonly nodeStates = new Map<string, NodeStateRecord>();
  private readonly enrollmentAttempts = new Map<
    string,
    EnrollmentAttemptRecord
  >();
  private readonly syncAttempts = new Map<string, SyncAttemptRecord>();
  private readonly appliedSnapshots = new Map<string, AppliedSnapshotRecord>();
  private readonly acknowledgements = new Map<
    string,
    SnapshotAcknowledgement
  >();

  async withTransaction<T>(
    work: (tx: ContinuityTransactionPort) => Promise<T>,
  ): Promise<T> {
    return work({
      getNodeState: async () => this.nodeStates.get("default"),
      saveNodeState: async (state) => {
        this.nodeStates.set(state.id, { ...state });
      },
      createEnrollmentAttempt: async (attempt) => {
        if (!isEnrollmentAttemptState(attempt.state)) {
          throw new Error(`invalid enrollment attempt state: ${attempt.state}`);
        }
        this.enrollmentAttempts.set(attempt.id, { ...attempt });
      },
      getEnrollmentAttempt: async (id) => this.enrollmentAttempts.get(id),
      recordSyncAttempt: async (attempt) => {
        this.syncAttempts.set(attempt.id, { ...attempt });
      },
      recordAppliedSnapshot: async (snapshot) => {
        this.appliedSnapshots.set(snapshot.id, { ...snapshot });
      },
      recordSnapshotAcknowledgement: async (ack) => {
        this.acknowledgements.set(`${ack.nodeId}:${ack.contentHash}`, {
          ...ack,
        });
      },
      getSnapshotAcknowledgement: async (nodeId, contentHash) =>
        this.acknowledgements.get(`${nodeId}:${contentHash}`),
    });
  }
}

function isActiveJobStatus(status: JobStatus): boolean {
  return (
    status === "queued" ||
    status === "validating" ||
    status === "applying"
  );
}

function isEnrollmentAttemptState(
  value: string,
): value is EnrollmentAttemptState {
  return [
    "draft",
    "verified",
    "confirmed",
    "exchanged",
    "archived",
    "initial-apply-pending",
    "committed",
    "cancelled",
    "recoverable",
    "failed",
  ].includes(value);
}

export type RevisionRecord = {
  id: string;
  revisionNumber: number;
  checksum: string;
  snapshot: ConfigurationSnapshot;
  actorUserId?: string;
  source?: PersistenceSource;
  sourcePrimaryId?: string;
  sourceNodeId?: string;
  sourceRevisionId?: string;
  snapshotContentHash?: string;
  snapshotVersion?: number;
  replicationVersion?: number;
  leadershipGeneration?: number;
  createdAt: Date;
  appliedAt?: Date;
};

export interface RevisionStore {
  create(
    snapshot: ConfigurationSnapshot,
    actorUserId?: string,
  ): Promise<RevisionRecord>;
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
  source?: PersistenceSource;
  sourcePrimaryId?: string;
  sourceNodeId?: string;
  sourceRevisionId?: string;
  snapshotContentHash?: string;
  snapshotVersion?: number;
  replicationVersion?: number;
  leadershipGeneration?: number;
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

export type JobEnqueueInput = Omit<
  JobRecord,
  "id" | "status" | "createdAt" | "source"
> & {
  status?: JobStatus;
  source?: PersistenceSource;
};

export interface JobStore {
  enqueue(job: JobEnqueueInput): Promise<JobRecord>;
  /**
   * Enqueue unless nonterminal work exists. This is used for reconciliation
   * reservations, so implementations must make the check and insert atomic
   * for their persistence model while preserving terminal retry semantics.
   */
  enqueueIfNotActive(job: JobEnqueueInput): Promise<JobRecord | undefined>;
  get(id: string): Promise<JobRecord | undefined>;
  claimNext(target?: JobTarget): Promise<JobRecord | undefined>;
  recoverStale(leaseMs: number, now?: Date): Promise<number>;
  update(id: string, patch: Partial<JobRecord>): Promise<JobRecord>;
  list(): Promise<JobRecord[]>;
}

export class InMemoryRevisionStore implements RevisionStore {
  private readonly records = new Map<string, RevisionRecord>();
  private nextNumber = 1;

  async create(
    snapshot: ConfigurationSnapshot,
    actorUserId?: string,
  ): Promise<RevisionRecord> {
    const normalized = createSnapshot(snapshot);
    const record: RevisionRecord = {
      id: randomUUID(),
      revisionNumber: this.nextNumber++,
      checksum: checksumSnapshot(normalized),
      snapshot: normalized,
      actorUserId,
      source: "ordinary",
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

  async markApplied(
    id: string,
    appliedAt = new Date(),
  ): Promise<RevisionRecord> {
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

  async enqueue(job: JobEnqueueInput): Promise<JobRecord> {
    const record: JobRecord = {
      ...job,
      id: randomUUID(),
      status: job.status ?? "queued",
      source: job.source ?? "ordinary",
      createdAt: new Date(),
    };
    this.records.set(record.id, record);
    if (record.status === "validating" || record.status === "applying") {
      this.activeTargets.add(record.target);
    }
    return record;
  }

  async enqueueIfNotActive(
    job: JobEnqueueInput,
  ): Promise<JobRecord | undefined> {
    const blocked = [...this.records.values()].some((existing) =>
      isActiveJobStatus(existing.status),
    );
    if (blocked) return undefined;
    return this.enqueue(job);
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
