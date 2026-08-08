import { randomUUID } from "node:crypto";
import {
  InMemorySecretStore,
  issueSelfSigned,
  resolveProxySettingsInput,
  type CertificateResult,
  type ProxySettingsInput,
} from "@proxycore/certificates";
import {
  InMemoryJobStore,
  InMemoryRevisionStore,
  type AutoApplyResult,
  type JobRecord,
  type RevisionRecord,
} from "@proxycore/db";
import {
  normalizeDnsName,
  validateForwardingRules,
  validateRecordSet,
  validateResolverPool,
  validateStreamRoutes,
  type DnsRecord,
  type DnsRecordInput,
  type ForwardingRule,
  type IngressAddresses,
  type ResolverPool,
  type StreamRoute,
} from "@proxycore/domain";
import type {
  CertificateStatus,
  ConfigurationSnapshot,
  InstallationSettings,
  ZoneState,
} from "@proxycore/domain";

export type RecordMutationInput = Omit<DnsRecordInput, "id" | "proxy"> & {
  id?: string;
  proxy?: ProxySettingsInput;
};

export type { CertificateStatus, InstallationSettings, ZoneState };

export interface ConfigurationStore {
  getSettings(): Promise<InstallationSettings>;
  initializeIngress(defaultIngress: IngressAddresses): Promise<boolean>;
  updateSettings(
    input: Partial<InstallationSettings>,
  ): Promise<InstallationSettings>;
  listZones(): Promise<ZoneState[]>;
  createZone(
    name: string,
    actorUserId: string,
  ): Promise<AutoApplyResult<ZoneState>>;
  getZone(id: string): Promise<ZoneState>;
  addRecord(
    zoneId: string,
    input: RecordMutationInput,
    actorUserId: string,
  ): Promise<AutoApplyResult<DnsRecord>>;
  listStreams(): Promise<StreamRoute[]>;
  addStream(
    input: Omit<StreamRoute, "id"> & { id?: string },
  ): Promise<StreamRoute>;
  listCertificates(): Promise<CertificateStatus[]>;
  issueCertificate(input: {
    hostnames: string[];
    issuer: "self-signed" | "letsencrypt";
    challenge: "none" | "http-01" | "dns-01";
    environment?: string;
  }): Promise<CertificateStatus>;
  createApplyJob(
    actorUserId: string,
  ): Promise<{ revisionId: string; job: JobRecord }>;
  status(): Promise<{
    desiredRevision?: RevisionRecord;
    appliedRevision?: RevisionRecord;
    jobs: JobRecord[];
    settings: InstallationSettings;
    zones: ZoneState[];
    streams: StreamRoute[];
    certificates: CertificateStatus[];
  }>;
  snapshot(): Promise<ConfigurationSnapshot>;
}

export class InMemoryConfigurationStore {
  private settings: InstallationSettings = {
    ingress: {},
    forwardingRules: [],
    retentionMaxAgeDays: 7,
    retentionMaxSizeMb: 50,
  };
  private zones: ZoneState[] = [];
  private streams: StreamRoute[] = [];
  private readonly certificates: CertificateStatus[] = [];
  private readonly revisions = new InMemoryRevisionStore();
  private readonly jobs = new InMemoryJobStore();
  private appliedRevisionId?: string;
  private readonly masterKeyBase64?: string;
  private readonly secretStore?: InMemorySecretStore;

  constructor(masterKeyBase64?: string, defaultIngress: IngressAddresses = {}) {
    this.masterKeyBase64 = masterKeyBase64;
    this.secretStore = masterKeyBase64
      ? new InMemorySecretStore(masterKeyBase64)
      : undefined;
    this.settings.ingress = clone(defaultIngress);
  }

  async getSettings(): Promise<InstallationSettings> {
    return clone(this.settings);
  }

  async initializeIngress(defaultIngress: IngressAddresses): Promise<boolean> {
    if (this.settings.ingress.ipv4 || this.settings.ingress.ipv6) return false;
    if (!defaultIngress.ipv4 && !defaultIngress.ipv6) return false;
    this.settings.ingress = clone(defaultIngress);
    return true;
  }

  async updateSettings(
    input: Partial<InstallationSettings>,
  ): Promise<InstallationSettings> {
    const next: InstallationSettings = {
      ...this.settings,
      ...input,
      ingress: { ...this.settings.ingress, ...(input.ingress ?? {}) },
      forwardingRules: input.forwardingRules
        ? validateForwardingRules(input.forwardingRules)
        : this.settings.forwardingRules,
      defaultPool: input.defaultPool
        ? validateResolverPool(input.defaultPool)
        : this.settings.defaultPool,
    };
    if (next.retentionMaxAgeDays < 1 || next.retentionMaxSizeMb < 1) {
      throw new Error("Retention limits must be positive");
    }
    this.settings = clone(next);
    return this.getSettings();
  }

  async listZones(): Promise<ZoneState[]> {
    return clone(this.zones);
  }

  async createZone(
    name: string,
    actorUserId: string,
  ): Promise<AutoApplyResult<ZoneState>> {
    if (!this.settings.defaultPool) {
      throw new Error("Configure a default resolver pool before saving DNS");
    }
    const normalized = normalizeDnsName(name, false);
    if (this.zones.some((zone) => zone.name === normalized)) {
      throw new Error("Zone already exists");
    }
    const zone: ZoneState = {
      id: randomUUID(),
      name: normalized,
      enabled: true,
      records: [],
    };
    this.zones.push(zone);
    const apply = await this.createApplyJob(actorUserId);
    return { value: clone(zone), apply };
  }

  async getZone(id: string): Promise<ZoneState> {
    const zone = this.zones.find((item) => item.id === id);
    if (!zone) throw new Error("Zone not found");
    return clone(zone);
  }

  async addRecord(
    zoneId: string,
    input: RecordMutationInput,
    actorUserId: string,
  ): Promise<AutoApplyResult<DnsRecord>> {
    if (!this.settings.defaultPool) {
      throw new Error("Configure a default resolver pool before saving DNS");
    }
    const zone = this.zones.find((item) => item.id === zoneId);
    if (!zone) throw new Error("Zone not found");
    const recordId = input.id ?? randomUUID();
    const existing = zone.records.find((item) => item.id === recordId);
    const proxy = await resolveProxySettingsInput(input.proxy, {
      secretStore: this.secretStore,
      existing: existing?.proxy,
    });
    const record: DnsRecordInput = {
      ...input,
      id: recordId,
      proxy,
    };
    const records = validateRecordSet(
      [...zone.records.filter((item) => item.id !== recordId), record],
      {
        zoneName: zone.name,
        ingress: this.settings.ingress,
        certificates: this.certificates,
      },
    );
    zone.records = records;
    const apply = await this.createApplyJob(actorUserId);
    return {
      value: clone(records.find((item) => item.id === record.id)!),
      apply,
    };
  }

  async listStreams(): Promise<StreamRoute[]> {
    return clone(this.streams);
  }

  async addStream(
    input: Omit<StreamRoute, "id"> & { id?: string },
  ): Promise<StreamRoute> {
    const route = { ...input, id: input.id ?? randomUUID() };
    this.streams = validateStreamRoutes([...this.streams, route]);
    return clone(this.streams.find((item) => item.id === route.id)!);
  }

  async listCertificates(): Promise<CertificateStatus[]> {
    return clone(this.certificates);
  }

  async issueCertificate(input: {
    hostnames: string[];
    issuer: "self-signed" | "letsencrypt";
    challenge: "none" | "http-01" | "dns-01";
    environment?: string;
  }): Promise<CertificateStatus> {
    const certificate: CertificateStatus = {
      id: randomUUID(),
      hostnames: input.hostnames,
      issuer: input.issuer,
      challenge: input.challenge,
      environment: input.environment ?? "staging",
      status: "pending",
    };
    if (input.issuer === "self-signed") {
      if (input.challenge !== "none" || !this.secretStore) {
        throw new Error(
          "Self-signed issuance requires challenge none and a master key",
        );
      }
      const result: CertificateResult = await issueSelfSigned(input.hostnames, {
        secretStore: this.secretStore,
        masterKeyBase64: this.masterKeyBase64 ?? "",
      });
      certificate.status = "active";
      certificate.expiresAt = result.expiresAt;
      certificate.secretId = result.secretId;
      certificate.certificatePem = result.certificatePem;
    }
    this.certificates.push(certificate);
    return clone(certificate);
  }

  async createApplyJob(
    actorUserId: string,
  ): Promise<{ revisionId: string; job: JobRecord }> {
    if (!this.settings.defaultPool) {
      throw new Error("Configure a default resolver pool before apply");
    }
    const revision = await this.revisions.create(
      await this.snapshot(),
      actorUserId,
    );
    const job = await this.jobs.enqueue({
      revisionId: revision.id,
      target: "combined",
      correlationId: randomUUID(),
    });
    return { revisionId: revision.id, job };
  }

  async status() {
    const desiredRevision = await this.revisions.latest();
    const appliedRevision = this.appliedRevisionId
      ? await this.revisions.get(this.appliedRevisionId)
      : undefined;
    return {
      desiredRevision,
      appliedRevision,
      jobs: await this.jobs.list(),
      settings: await this.getSettings(),
      zones: await this.listZones(),
      streams: await this.listStreams(),
      certificates: await this.listCertificates(),
    };
  }

  async snapshot(): Promise<ConfigurationSnapshot> {
    return {
      settings: await this.getSettings(),
      zones: await this.listZones(),
      streams: await this.listStreams(),
      certificates: await this.listCertificates(),
    };
  }
}

function clone<T>(value: T): T {
  return structuredClone(value);
}
