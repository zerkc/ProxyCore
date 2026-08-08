import {
  materializeBasicAuthFiles,
  materializeCertificateFiles,
  type SecretStore,
} from "@proxycore/certificates";
import {
  renderCoreDnsCandidate,
  renderNginxCandidate,
  type CoreDnsCandidate,
  type NginxCandidate,
} from "@proxycore/renderers";
import type {
  CertificateStatus,
  ConfigurationSnapshot,
  DnsRecord,
  Http3Capabilities,
  InstallationSettings,
  StreamRoute,
  ZoneState,
} from "@proxycore/domain";
import type { JobRecord } from "@proxycore/db";
import type { RenderedCandidate } from "./apply";

export type WorkerRenderOptions = {
  candidateRoot?: string;
  capabilities?: Http3Capabilities;
  secretStore?: SecretStore;
};

export async function renderJobCandidates(
  snapshot: ConfigurationSnapshot,
  job: JobRecord,
  options: WorkerRenderOptions = {},
): Promise<RenderedCandidate | RenderedCandidate[]> {
  const desired = snapshot as {
    settings?: InstallationSettings;
    zones?: ZoneState[];
    streams?: StreamRoute[];
    certificates?: CertificateStatus[];
  };
  const settings = desired.settings;
  if (!settings?.defaultPool) {
    throw new Error("Configuration snapshot has no default resolver pool");
  }
  const zones = (desired.zones ?? []).filter((zone) => zone.enabled);
  const records = zones.flatMap((zone) => zone.records);
  const certificates = desired.certificates ?? [];
  const candidateRoot =
    options.candidateRoot ?? "/var/lib/proxycore/candidates";
  const capabilities = options.capabilities ?? {
    http3Module: false,
    tcp443Published: false,
    udp443Published: false,
  };
  const renderers = {
    coredns: () => {
      const candidate = renderCoreDnsCandidate({
        zones: zones.map((zone) => ({
          name: zone.name,
          records: zone.records,
        })),
        ingress: settings.ingress,
        defaultPool: settings.defaultPool!,
        forwardingRules: settings.forwardingRules,
      });
      return coreDnsCandidate(candidate, candidateRoot, job);
    },
    nginx: async () => {
      const candidatePath = candidatePathFor(candidateRoot, job, "nginx");
      const secretFiles = options.secretStore
        ? {
            ...(await materializeBasicAuthFiles(records, options.secretStore)),
            ...(await materializeCertificateFiles(
              records,
              certificates,
              options.secretStore,
            )),
          }
        : {
            ...(await requireNoBasicAuth(records)),
            ...(await requireNoTlsCertificates(records)),
          };
      const candidate = renderNginxCandidate({
        records,
        streams: desired.streams ?? [],
        capabilities,
        candidatePath,
        extraFiles: secretFiles,
      });
      return nginxCandidate(candidate, candidatePath);
    },
  };

  if (job.target === "coredns") return renderers.coredns();
  if (job.target === "nginx") return renderers.nginx();
  if (job.target === "combined")
    return [renderers.coredns(), await renderers.nginx()];
  throw new Error(`Unsupported apply target: ${job.target}`);
}

function coreDnsCandidate(
  candidate: CoreDnsCandidate,
  candidateRoot: string,
  job: JobRecord,
): RenderedCandidate {
  return {
    service: "coredns",
    candidatePath: candidatePathFor(candidateRoot, job, "coredns"),
    checksum: candidate.checksum,
    files: { Corefile: candidate.corefile, ...candidate.files },
  };
}

function nginxCandidate(
  candidate: NginxCandidate,
  candidatePath: string,
): RenderedCandidate {
  return {
    service: "nginx",
    candidatePath,
    checksum: candidate.checksum,
    files: candidate.files,
  };
}

function candidatePathFor(
  candidateRoot: string,
  job: JobRecord,
  service: "coredns" | "nginx",
): string {
  if (!/^[a-zA-Z0-9_-]+$/.test(job.revisionId)) {
    throw new Error("Revision ID is invalid for a candidate path");
  }
  return `${candidateRoot.replace(/\/+$/, "")}/${job.revisionId}/${service}`;
}

async function requireNoBasicAuth(
  records: DnsRecord[],
): Promise<Record<string, string>> {
  const hasBasicAuth = records.some((record) => record.proxy?.basicAuth);
  if (hasBasicAuth) {
    throw new Error("Basic Auth secrets require a configured secret store");
  }
  return {};
}

async function requireNoTlsCertificates(
  records: DnsRecord[],
): Promise<Record<string, string>> {
  const needsTls = records.some(
    (record) => record.proxied && record.proxy?.tlsEnabled,
  );
  if (needsTls) {
    throw new Error("TLS certificates require a configured secret store");
  }
  return {};
}
