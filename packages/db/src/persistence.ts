import { randomUUID } from "node:crypto";
import { and, asc, desc, eq, lt, or, sql } from "drizzle-orm";
import {
  issueSelfSigned,
  resolveProxySettingsInput,
  type ProxySettingsInput,
  type SecretStore,
} from "@proxycore/certificates";
import {
  createSnapshot,
  checksumSnapshot,
  normalizeDnsName,
  validateForwardingRules,
  validateRecordSet,
  validateResolverPool,
  validateStreamRoutes,
  type CertificateStatus,
  type ConfigurationSnapshot,
  type DnsRecord,
  type DnsRecordInput,
  type ForwardingRule,
  type IngressAddresses,
  type InstallationSettings,
  type JobStatus,
  type ResolverPool,
  type StreamRoute,
  type ZoneState,
} from "@proxycore/domain";

export type RecordMutationInput = Omit<DnsRecordInput, "id" | "proxy"> & {
  id?: string;
  proxy?: ProxySettingsInput;
};
import type { ProxyCoreDatabase } from "./index";
import {
  applyJobs,
  certificates,
  configRevisions,
  dnsRecords,
  installationSettings,
  streamRoutes,
  zones,
} from "./schema";
import type {
  JobRecord,
  JobStore,
  JobTarget,
  RevisionRecord,
  RevisionStore,
} from "./ports";
import { JOB_NOTIFICATION_CHANNEL } from "./notifications";
import { PgSecretStore } from "./secret-store";

const INSTALLATION_ID = "default";
const REVISION_LOCK_KEY = 1_872_641;

type DbExecutor = Pick<
  ProxyCoreDatabase,
  "select" | "insert" | "update" | "delete"
>;
type NotificationExecutor = Pick<ProxyCoreDatabase, "execute">;
type DbSession = DbExecutor & NotificationExecutor;

export type AutoApplyResult<T> = {
  value: T;
  apply: {
    revisionId: string;
    job: JobRecord;
  };
};

export async function notifyJob(
  db: NotificationExecutor,
  jobId: string,
): Promise<void> {
  await db.execute(
    sql`SELECT pg_notify(${JOB_NOTIFICATION_CHANNEL}, ${jobId})`,
  );
}

export class PgRevisionStore implements RevisionStore {
  constructor(private readonly db: ProxyCoreDatabase) {}

  async create(
    snapshot: ConfigurationSnapshot,
    actorUserId?: string,
  ): Promise<RevisionRecord> {
    const normalized = createSnapshot(snapshot);
    const checksum = checksumSnapshot(normalized);
    const existing = await this.db
      .select()
      .from(configRevisions)
      .where(eq(configRevisions.checksum, checksum))
      .limit(1);
    if (existing[0]) {
      return toRevision(existing[0]);
    }

    const latest = await this.db
      .select({ revisionNumber: configRevisions.revisionNumber })
      .from(configRevisions)
      .orderBy(desc(configRevisions.revisionNumber))
      .limit(1);
    const [row] = await this.db
      .insert(configRevisions)
      .values({
        id: randomUUID(),
        revisionNumber: (latest[0]?.revisionNumber ?? 0) + 1,
        checksum,
        snapshot: normalized,
        actorUserId,
      })
      .returning();
    return toRevision(row);
  }

  async get(id: string): Promise<RevisionRecord | undefined> {
    const [row] = await this.db
      .select()
      .from(configRevisions)
      .where(eq(configRevisions.id, id))
      .limit(1);
    return row ? toRevision(row) : undefined;
  }

  async latest(): Promise<RevisionRecord | undefined> {
    const [row] = await this.db
      .select()
      .from(configRevisions)
      .orderBy(desc(configRevisions.revisionNumber))
      .limit(1);
    return row ? toRevision(row) : undefined;
  }

  async markApplied(
    id: string,
    appliedAt = new Date(),
  ): Promise<RevisionRecord> {
    const [row] = await this.db
      .update(configRevisions)
      .set({ appliedAt })
      .where(eq(configRevisions.id, id))
      .returning();
    if (!row) {
      throw new Error(`Revision not found: ${id}`);
    }
    await ensureSettings(this.db);
    await this.db
      .update(installationSettings)
      .set({ currentAppliedRevisionId: id, updatedAt: appliedAt })
      .where(eq(installationSettings.id, INSTALLATION_ID));
    return toRevision(row);
  }
}

export class PgJobStore implements JobStore {
  constructor(private readonly db: ProxyCoreDatabase) {}

  async enqueue(
    job: Omit<JobRecord, "id" | "status" | "createdAt"> & {
      status?: JobStatus;
    },
  ): Promise<JobRecord> {
    return this.db.transaction(async (tx) => {
      const [row] = await tx
        .insert(applyJobs)
        .values({
          id: randomUUID(),
          revisionId: job.revisionId,
          actorUserId: job.actorUserId,
          target: job.target,
          status: job.status ?? "queued",
          correlationId: job.correlationId,
          validationOutput: job.validationOutput,
          applyOutput: job.applyOutput,
          healthOutput: job.healthOutput,
          errorMessage: job.errorMessage,
          claimedAt: job.claimedAt,
          startedAt: job.startedAt,
          finishedAt: job.finishedAt,
        })
        .returning();
      await notifyJob(tx, row.id);
      return toJob(row);
    });
  }

  async get(id: string): Promise<JobRecord | undefined> {
    const [row] = await this.db
      .select()
      .from(applyJobs)
      .where(eq(applyJobs.id, id))
      .limit(1);
    return row ? toJob(row) : undefined;
  }

  async claimNext(target?: JobTarget): Promise<JobRecord | undefined> {
    return this.db.transaction(async (tx) => {
      await tx.execute(sql`SELECT pg_advisory_xact_lock(${REVISION_LOCK_KEY})`);
      const rows = await tx
        .select()
        .from(applyJobs)
        .where(
          target
            ? and(eq(applyJobs.status, "queued"), eq(applyJobs.target, target))
            : eq(applyJobs.status, "queued"),
        )
        .orderBy(asc(applyJobs.createdAt))
        .limit(1)
        .for("update", { skipLocked: true });
      const row = rows[0];
      if (!row) return undefined;
      const now = new Date();
      const [claimed] = await tx
        .update(applyJobs)
        .set({
          status: "validating",
          claimedAt: now,
          startedAt: row.startedAt ?? now,
        })
        .where(eq(applyJobs.id, row.id))
        .returning();
      return claimed ? toJob(claimed) : undefined;
    });
  }

  async recoverStale(leaseMs: number, now = new Date()): Promise<number> {
    const cutoff = new Date(now.getTime() - leaseMs);
    const recovered = await this.db
      .update(applyJobs)
      .set({
        status: "queued",
        claimedAt: null,
        startedAt: null,
        errorMessage: "Recovered after worker lease expiration",
      })
      .where(
        and(
          or(
            eq(applyJobs.status, "validating"),
            eq(applyJobs.status, "applying"),
          ),
          lt(applyJobs.claimedAt, cutoff),
        ),
      )
      .returning({ id: applyJobs.id });
    return recovered.length;
  }

  async update(id: string, patch: Partial<JobRecord>): Promise<JobRecord> {
    const values: Partial<typeof applyJobs.$inferInsert> = {};
    if (patch.status !== undefined) values.status = patch.status;
    if (patch.validationOutput !== undefined)
      values.validationOutput = patch.validationOutput;
    if (patch.applyOutput !== undefined) values.applyOutput = patch.applyOutput;
    if (patch.healthOutput !== undefined)
      values.healthOutput = patch.healthOutput;
    if (patch.errorMessage !== undefined)
      values.errorMessage = patch.errorMessage;
    if (patch.claimedAt !== undefined) values.claimedAt = patch.claimedAt;
    if (patch.startedAt !== undefined) values.startedAt = patch.startedAt;
    if (patch.finishedAt !== undefined) values.finishedAt = patch.finishedAt;
    const [row] = await this.db
      .update(applyJobs)
      .set(values)
      .where(eq(applyJobs.id, id))
      .returning();
    if (!row) throw new Error(`Job not found: ${id}`);
    return toJob(row);
  }

  async list(): Promise<JobRecord[]> {
    const rows = await this.db
      .select()
      .from(applyJobs)
      .orderBy(desc(applyJobs.createdAt));
    return rows.map(toJob);
  }
}

export class PgConfigurationStore {
  private readonly revisions: PgRevisionStore;
  private readonly jobs: PgJobStore;
  private readonly secretStore?: SecretStore;
  private readonly masterKeyBase64?: string;

  constructor(
    private readonly db: ProxyCoreDatabase,
    masterKeyBase64?: string,
    private readonly defaultIngress: IngressAddresses = {},
  ) {
    this.masterKeyBase64 = masterKeyBase64;
    this.revisions = new PgRevisionStore(db);
    this.jobs = new PgJobStore(db);
    this.secretStore = masterKeyBase64
      ? new PgSecretStore(db, masterKeyBase64)
      : undefined;
  }

  async getSettings(): Promise<InstallationSettings> {
    await this.initializeIngress(this.defaultIngress);
    return toSettings(await ensureSettings(this.db));
  }

  async initializeIngress(defaultIngress: IngressAddresses): Promise<boolean> {
    if (!defaultIngress.ipv4 && !defaultIngress.ipv6) return false;
    return this.db.transaction(async (tx) => {
      const current = await ensureSettings(tx);
      if (current.ingressIpv4 || current.ingressIpv6) return false;
      await tx
        .update(installationSettings)
        .set({
          ingressIpv4: defaultIngress.ipv4 ?? null,
          ingressIpv6: defaultIngress.ipv6 ?? null,
          updatedAt: new Date(),
        })
        .where(eq(installationSettings.id, INSTALLATION_ID));
      return true;
    });
  }

  async updateSettings(
    input: Partial<InstallationSettings>,
  ): Promise<InstallationSettings> {
    const current = await this.getSettings();
    const next: InstallationSettings = {
      ...current,
      ...input,
      ingress: { ...current.ingress, ...(input.ingress ?? {}) },
      forwardingRules: input.forwardingRules
        ? validateForwardingRules(input.forwardingRules)
        : current.forwardingRules,
      defaultPool: input.defaultPool
        ? validateResolverPool(input.defaultPool)
        : current.defaultPool,
    };
    if (next.retentionMaxAgeDays < 1 || next.retentionMaxSizeMb < 1) {
      throw new Error("Retention limits must be positive");
    }
    await ensureSettings(this.db);
    await this.db
      .update(installationSettings)
      .set({
        ingressIpv4: next.ingress.ipv4 ?? null,
        ingressIpv6: next.ingress.ipv6 ?? null,
        defaultResolverPool: next.defaultPool ?? null,
        forwardingRules: next.forwardingRules,
        retentionMaxAgeDays: next.retentionMaxAgeDays,
        retentionMaxSizeMb: next.retentionMaxSizeMb,
        updatedAt: new Date(),
      })
      .where(eq(installationSettings.id, INSTALLATION_ID));
    return next;
  }

  async listZones(): Promise<ZoneState[]> {
    const [zoneRows, recordRows] = await Promise.all([
      this.db.select().from(zones).orderBy(asc(zones.name)),
      this.db.select().from(dnsRecords).orderBy(asc(dnsRecords.name)),
    ]);
    const recordsByZone = new Map<string, DnsRecord[]>();
    for (const row of recordRows) {
      const records = recordsByZone.get(row.zoneId) ?? [];
      records.push(toRecord(row));
      recordsByZone.set(row.zoneId, records);
    }
    return zoneRows.map((row) => ({
      id: row.id,
      name: row.name,
      enabled: row.enabled,
      records: recordsByZone.get(row.id) ?? [],
    }));
  }

  async createZone(
    name: string,
    actorUserId: string,
  ): Promise<AutoApplyResult<ZoneState>> {
    return this.db.transaction(async (tx) => {
      const normalized = normalizeDnsName(name, false);
      const [row] = await tx
        .insert(zones)
        .values({ id: randomUUID(), name: normalized, enabled: true })
        .returning();
      const apply = await createApplyJobInTransaction(tx, actorUserId);
      return {
        value: {
          id: row.id,
          name: row.name,
          enabled: row.enabled,
          records: [],
        },
        apply,
      };
    });
  }

  async getZone(id: string): Promise<ZoneState> {
    const [zone] = await this.db
      .select()
      .from(zones)
      .where(eq(zones.id, id))
      .limit(1);
    if (!zone) throw new Error("Zone not found");
    const records = await this.db
      .select()
      .from(dnsRecords)
      .where(eq(dnsRecords.zoneId, id))
      .orderBy(asc(dnsRecords.name));
    return {
      id: zone.id,
      name: zone.name,
      enabled: zone.enabled,
      records: records.map(toRecord),
    };
  }

  async addRecord(
    zoneId: string,
    input: RecordMutationInput,
    actorUserId: string,
  ): Promise<AutoApplyResult<DnsRecord>> {
    return this.db.transaction(async (tx) => {
      const [zone] = await tx
        .select()
        .from(zones)
        .where(eq(zones.id, zoneId))
        .limit(1)
        .for("update");
      if (!zone) throw new Error("Zone not found");
      const currentRows = await tx
        .select()
        .from(dnsRecords)
        .where(eq(dnsRecords.zoneId, zoneId));
      const certificateRows = await tx.select().from(certificates);
      const recordId = input.id ?? randomUUID();
      const existing = currentRows.find((row) => row.id === recordId);
      const proxy = await resolveProxySettingsInput(input.proxy, {
        secretStore: this.secretStore,
        existing: existing ? toRecord(existing).proxy : undefined,
      });
      const record: DnsRecordInput = {
        ...input,
        id: recordId,
        proxy,
      };
      const validated = validateRecordSet(
        [
          ...currentRows.filter((row) => row.id !== record.id).map(toRecord),
          record,
        ],
        {
          zoneName: zone.name,
          ingress: await this.getIngress(tx),
          certificates: certificateRows.map(toCertificateStatus),
        },
      );
      for (const item of validated) {
        await tx
          .insert(dnsRecords)
          .values(toRecordInsert(zoneId, item))
          .onConflictDoUpdate({
            target: dnsRecords.id,
            set: toRecordUpdate(item),
          });
      }
      const apply = await createApplyJobInTransaction(tx, actorUserId);
      return {
        value: validated.find((item) => item.id === record.id)!,
        apply,
      };
    });
  }

  async listStreams(): Promise<StreamRoute[]> {
    const rows = await this.db
      .select()
      .from(streamRoutes)
      .orderBy(asc(streamRoutes.listenPort));
    return validateStreamRoutes(
      rows.map((row) => ({
        id: row.id,
        enabled: row.enabled,
        protocol: row.protocol,
        listenAddress: row.listenAddress,
        listenPort: row.listenPort,
        upstream: row.upstream as StreamRoute["upstream"],
      })),
    );
  }

  async addStream(
    input: Omit<StreamRoute, "id"> & { id?: string },
  ): Promise<StreamRoute> {
    const current = await this.listStreams();
    const route = { ...input, id: input.id ?? randomUUID() };
    const validated = validateStreamRoutes([...current, route]);
    const saved = validated.find((item) => item.id === route.id)!;
    await this.db
      .insert(streamRoutes)
      .values({
        id: saved.id,
        enabled: saved.enabled,
        protocol: saved.protocol,
        listenAddress: saved.listenAddress,
        listenPort: saved.listenPort,
        upstream: saved.upstream,
      })
      .onConflictDoUpdate({
        target: streamRoutes.id,
        set: {
          enabled: saved.enabled,
          protocol: saved.protocol,
          listenAddress: saved.listenAddress,
          listenPort: saved.listenPort,
          upstream: saved.upstream,
          updatedAt: new Date(),
        },
      });
    return saved;
  }

  async listCertificates(): Promise<CertificateStatus[]> {
    const rows = await this.db
      .select()
      .from(certificates)
      .orderBy(desc(certificates.createdAt));
    return rows.map(toCertificateStatus);
  }

  async issueCertificate(input: {
    hostnames: string[];
    issuer: "self-signed" | "letsencrypt";
    challenge: "none" | "http-01" | "dns-01";
    environment?: string;
  }): Promise<CertificateStatus> {
    const id = randomUUID();
    if (input.issuer === "self-signed") {
      if (input.challenge !== "none" || !this.secretStore) {
        throw new Error(
          "Self-signed issuance requires challenge none and a master key",
        );
      }
      const result = await issueSelfSigned(input.hostnames, {
        secretStore: this.secretStore,
        masterKeyBase64: this.masterKeyBase64 ?? "",
      });
      const [row] = await this.db
        .insert(certificates)
        .values({
          id,
          hostnames: input.hostnames,
          issuer: input.issuer,
          challenge: input.challenge,
          environment: input.environment ?? "staging",
          status: "active",
          expiresAt: result.expiresAt,
          renewAfter: new Date(
            result.expiresAt.getTime() - 30 * 24 * 60 * 60 * 1_000,
          ),
          keySecretId: result.secretId,
          certificatePem: result.certificatePem,
        })
        .returning();
      return toCertificateStatus(row);
    }

    const [row] = await this.db
      .insert(certificates)
      .values({
        id,
        hostnames: input.hostnames,
        issuer: input.issuer,
        challenge: input.challenge,
        environment: input.environment ?? "staging",
        status: "pending",
      })
      .returning();
    return toCertificateStatus(row);
  }

  async createApplyJob(
    actorUserId: string,
  ): Promise<{ revisionId: string; job: JobRecord }> {
    return this.db.transaction((tx) =>
      createApplyJobInTransaction(tx, actorUserId),
    );
  }

  async status() {
    const settingsRow = await ensureSettings(this.db);
    const [
      desiredRevision,
      appliedRevision,
      jobs,
      settings,
      zones,
      streams,
      certificates,
    ] = await Promise.all([
      settingsRow.currentDesiredRevisionId
        ? this.revisions.get(settingsRow.currentDesiredRevisionId)
        : undefined,
      settingsRow.currentAppliedRevisionId
        ? this.revisions.get(settingsRow.currentAppliedRevisionId)
        : undefined,
      this.jobs.list(),
      this.getSettings(),
      this.listZones(),
      this.listStreams(),
      this.listCertificates(),
    ]);
    return {
      desiredRevision,
      appliedRevision,
      jobs,
      settings,
      zones,
      streams,
      certificates,
    };
  }

  async snapshot(): Promise<ConfigurationSnapshot> {
    return readDesiredSnapshot(this.db);
  }

  getRevisionStore(): RevisionStore {
    return this.revisions;
  }

  getJobStore(): JobStore {
    return this.jobs;
  }

  private async getIngress(db: DbExecutor) {
    const row = await ensureSettings(db);
    return {
      ipv4: row.ingressIpv4 ?? undefined,
      ipv6: row.ingressIpv6 ?? undefined,
    };
  }
}

async function createApplyJobInTransaction(
  tx: DbSession,
  actorUserId: string,
): Promise<{ revisionId: string; job: JobRecord }> {
  await ensureSettings(tx);
  await tx.execute(sql`SELECT pg_advisory_xact_lock(${REVISION_LOCK_KEY})`);
  const settings = await tx
    .select()
    .from(installationSettings)
    .where(eq(installationSettings.id, INSTALLATION_ID))
    .limit(1)
    .for("update");
  const snapshot = await readDesiredSnapshot(tx);
  const snapshotSettings = snapshot.settings as InstallationSettings;
  if (!snapshotSettings.defaultPool) {
    throw new Error("Configure a default resolver pool before apply");
  }
  const normalized = createSnapshot(snapshot);
  const checksum = checksumSnapshot(normalized);
  const existing = await tx
    .select()
    .from(configRevisions)
    .where(eq(configRevisions.checksum, checksum))
    .limit(1);
  const revisionRow =
    existing[0] ??
    (
      await tx
        .insert(configRevisions)
        .values({
          id: randomUUID(),
          revisionNumber:
            ((
              await tx
                .select({ revisionNumber: configRevisions.revisionNumber })
                .from(configRevisions)
                .orderBy(desc(configRevisions.revisionNumber))
                .limit(1)
            )[0]?.revisionNumber ?? 0) + 1,
          checksum,
          snapshot: normalized,
          actorUserId,
        })
        .returning()
    )[0];
  const revisionId = revisionRow.id;
  await tx
    .update(installationSettings)
    .set({ currentDesiredRevisionId: revisionId, updatedAt: new Date() })
    .where(eq(installationSettings.id, settings[0].id));
  const [jobRow] = await tx
    .insert(applyJobs)
    .values({
      id: randomUUID(),
      revisionId,
      actorUserId,
      target: "combined",
      status: "queued",
      correlationId: randomUUID(),
    })
    .returning();
  await notifyJob(tx, jobRow.id);
  return { revisionId, job: toJob(jobRow) };
}

async function ensureSettings(
  db: DbExecutor,
): Promise<typeof installationSettings.$inferSelect> {
  const existing = await db
    .select()
    .from(installationSettings)
    .where(eq(installationSettings.id, INSTALLATION_ID))
    .limit(1);
  if (existing[0]) return existing[0];
  const [created] = await db
    .insert(installationSettings)
    .values({
      id: INSTALLATION_ID,
      forwardingRules: [],
      defaultResolverPool: null,
    })
    .onConflictDoNothing()
    .returning();
  if (created) return created;
  const [row] = await db
    .select()
    .from(installationSettings)
    .where(eq(installationSettings.id, INSTALLATION_ID))
    .limit(1);
  if (!row) throw new Error("Installation settings could not be initialized");
  return row;
}

async function readDesiredSnapshot(
  db: DbExecutor,
): Promise<ConfigurationSnapshot> {
  const [settingsRow, zoneRows, recordRows, streamRows, certificateRows] =
    await Promise.all([
      ensureSettings(db),
      db.select().from(zones).orderBy(asc(zones.name)),
      db.select().from(dnsRecords).orderBy(asc(dnsRecords.name)),
      db.select().from(streamRoutes).orderBy(asc(streamRoutes.listenPort)),
      db.select().from(certificates).orderBy(asc(certificates.id)),
    ]);
  const recordsByZone = new Map<string, DnsRecord[]>();
  for (const row of recordRows) {
    const records = recordsByZone.get(row.zoneId) ?? [];
    records.push(toRecord(row));
    recordsByZone.set(row.zoneId, records);
  }
  const settings = toSettings(settingsRow);
  return {
    settings,
    zones: zoneRows.map((row) => ({
      id: row.id,
      name: row.name,
      enabled: row.enabled,
      records: recordsByZone.get(row.id) ?? [],
    })),
    streams: validateStreamRoutes(
      streamRows.map((row) => ({
        id: row.id,
        enabled: row.enabled,
        protocol: row.protocol,
        listenAddress: row.listenAddress,
        listenPort: row.listenPort,
        upstream: row.upstream as StreamRoute["upstream"],
      })),
    ),
    certificates: certificateRows.map(toCertificateStatus),
  };
}

function toSettings(
  row: typeof installationSettings.$inferSelect,
): InstallationSettings {
  const defaultPool = parseDefaultPool(row.defaultResolverPool);
  const forwardingRules = parseForwardingRules(row.forwardingRules);
  return {
    ingress: {
      ipv4: row.ingressIpv4 ?? undefined,
      ipv6: row.ingressIpv6 ?? undefined,
    },
    defaultPool,
    forwardingRules,
    retentionMaxAgeDays: row.retentionMaxAgeDays,
    retentionMaxSizeMb: row.retentionMaxSizeMb,
  };
}

function toCertificateStatus(
  row: typeof certificates.$inferSelect,
): CertificateStatus {
  return {
    id: row.id,
    hostnames: row.hostnames,
    issuer: row.issuer,
    challenge: row.challenge,
    environment: row.environment,
    status: row.status,
    expiresAt: row.expiresAt ?? undefined,
    renewAfter: row.renewAfter ?? undefined,
    secretId: row.keySecretId ?? undefined,
    certificatePem: row.certificatePem ?? undefined,
  };
}

function parseDefaultPool(value: unknown): ResolverPool | undefined {
  if (!value || typeof value !== "object" || Array.isArray(value))
    return undefined;
  const candidate = value as { id?: unknown; endpoints?: unknown };
  if (typeof candidate.id !== "string" || !Array.isArray(candidate.endpoints)) {
    return undefined;
  }
  return validateResolverPool({
    id: candidate.id,
    endpoints: candidate.endpoints as ResolverPool["endpoints"],
  });
}

function parseForwardingRules(value: unknown): ForwardingRule[] {
  if (!Array.isArray(value)) return [];
  return validateForwardingRules(value as ForwardingRule[]);
}

function toRecord(row: typeof dnsRecords.$inferSelect): DnsRecord {
  return {
    id: row.id,
    name: row.name,
    type: row.type,
    value: row.value as DnsRecord["value"],
    ttl: row.ttl,
    enabled: row.enabled,
    comment: row.comment ?? undefined,
    proxied: row.proxied,
    proxy: row.proxySettings as DnsRecord["proxy"],
  };
}

function toRecordInsert(
  zoneId: string,
  record: DnsRecord,
): typeof dnsRecords.$inferInsert {
  return {
    id: record.id,
    zoneId,
    name: record.name,
    type: record.type,
    value: record.value,
    ttl: record.ttl,
    enabled: record.enabled,
    proxied: record.proxied,
    proxySettings: record.proxy,
    comment: record.comment,
  };
}

function toRecordUpdate(
  record: DnsRecord,
): Partial<typeof dnsRecords.$inferInsert> {
  return {
    name: record.name,
    type: record.type,
    value: record.value,
    ttl: record.ttl,
    enabled: record.enabled,
    proxied: record.proxied,
    proxySettings: record.proxy,
    comment: record.comment,
    updatedAt: new Date(),
  };
}

function toRevision(row: typeof configRevisions.$inferSelect): RevisionRecord {
  return {
    id: row.id,
    revisionNumber: row.revisionNumber,
    checksum: row.checksum,
    snapshot: row.snapshot as ConfigurationSnapshot,
    actorUserId: row.actorUserId ?? undefined,
    createdAt: row.createdAt,
    appliedAt: row.appliedAt ?? undefined,
  };
}

function toJob(row: typeof applyJobs.$inferSelect): JobRecord {
  return {
    id: row.id,
    revisionId: row.revisionId,
    actorUserId: row.actorUserId ?? undefined,
    target: row.target,
    status: row.status,
    correlationId: row.correlationId,
    createdAt: row.createdAt,
    claimedAt: row.claimedAt ?? undefined,
    startedAt: row.startedAt ?? undefined,
    finishedAt: row.finishedAt ?? undefined,
    validationOutput: row.validationOutput ?? undefined,
    applyOutput: row.applyOutput ?? undefined,
    healthOutput: row.healthOutput ?? undefined,
    errorMessage: row.errorMessage ?? undefined,
  };
}
