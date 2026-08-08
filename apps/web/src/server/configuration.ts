import { randomUUID } from "node:crypto";
import {
  InMemorySecretStore,
  issueSelfSigned,
  type CertificateResult,
} from "@proxycore/certificates";
import {
  InMemoryJobStore,
  InMemoryRevisionStore,
  type JobRecord,
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

export type InstallationSettings = {
  ingress: IngressAddresses;
  defaultPool?: ResolverPool;
  forwardingRules: ForwardingRule[];
  retentionMaxAgeDays: number;
  retentionMaxSizeMb: number;
};

export type ZoneState = {
  id: string;
  name: string;
  enabled: boolean;
  records: DnsRecord[];
};

export type CertificateStatus = {
  id: string;
  hostnames: string[];
  issuer: "self-signed" | "letsencrypt";
  challenge: "none" | "http-01" | "dns-01";
  environment: string;
  status: "pending" | "issued" | "active" | "failed";
  expiresAt?: Date;
  secretId?: string;
};

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

  constructor(masterKeyBase64?: string) {
    this.masterKeyBase64 = masterKeyBase64;
    this.secretStore = masterKeyBase64
      ? new InMemorySecretStore(masterKeyBase64)
      : undefined;
  }

  getSettings(): InstallationSettings {
    return clone(this.settings);
  }

  updateSettings(input: Partial<InstallationSettings>): InstallationSettings {
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

  listZones(): ZoneState[] {
    return clone(this.zones);
  }

  createZone(name: string): ZoneState {
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
    return clone(zone);
  }

  getZone(id: string): ZoneState {
    const zone = this.zones.find((item) => item.id === id);
    if (!zone) throw new Error("Zone not found");
    return clone(zone);
  }

  addRecord(
    zoneId: string,
    input: Omit<DnsRecordInput, "id"> & { id?: string },
  ): DnsRecord {
    const zone = this.zones.find((item) => item.id === zoneId);
    if (!zone) throw new Error("Zone not found");
    const record: DnsRecordInput = {
      ...input,
      id: input.id ?? randomUUID(),
    };
    const records = validateRecordSet([...zone.records, record], {
      zoneName: zone.name,
      ingress: this.settings.ingress,
    });
    zone.records = records;
    return clone(records.find((item) => item.id === record.id)!);
  }

  listStreams(): StreamRoute[] {
    return clone(this.streams);
  }

  addStream(input: Omit<StreamRoute, "id"> & { id?: string }): StreamRoute {
    const route = { ...input, id: input.id ?? randomUUID() };
    this.streams = validateStreamRoutes([...this.streams, route]);
    return clone(this.streams.find((item) => item.id === route.id)!);
  }

  listCertificates(): CertificateStatus[] {
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
        throw new Error("Self-signed issuance requires challenge none and a master key");
      }
      const result: CertificateResult = await issueSelfSigned(input.hostnames, {
        secretStore: this.secretStore,
        masterKeyBase64: this.masterKeyBase64 ?? "",
      });
      certificate.status = "active";
      certificate.expiresAt = result.expiresAt;
      certificate.secretId = result.secretId;
    }
    this.certificates.push(certificate);
    return clone(certificate);
  }

  createApplyJob(actorUserId: string): { revisionId: string; job: JobRecord } {
    if (!this.settings.defaultPool) {
      throw new Error("Configure a default resolver pool before apply");
    }
    const revision = this.revisions.create(this.snapshot(), actorUserId);
    const job = this.jobs.enqueue({
      revisionId: revision.id,
      target: "combined",
      correlationId: randomUUID(),
    });
    return { revisionId: revision.id, job };
  }

  status() {
    return {
      desiredRevision: this.revisions.latest(),
      appliedRevision: this.appliedRevisionId
        ? this.revisions.get(this.appliedRevisionId)
        : undefined,
      jobs: this.jobs.list(),
      settings: this.getSettings(),
      zones: this.listZones(),
      streams: this.listStreams(),
      certificates: this.listCertificates(),
    };
  }

  snapshot(): Record<string, unknown> {
    return {
      settings: this.getSettings(),
      zones: this.listZones(),
      streams: this.listStreams(),
      certificates: this.listCertificates(),
    };
  }
}

function clone<T>(value: T): T {
  return structuredClone(value);
}
