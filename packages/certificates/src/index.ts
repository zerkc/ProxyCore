import {
  createPrivateKey,
  createPublicKey,
  randomUUID,
  X509Certificate,
} from "node:crypto";
import { isIP } from "node:net";
import * as acme from "acme-client";
import * as selfsigned from "selfsigned";
import { decryptSecret, encryptSecret } from "@proxycore/crypto";
import { normalizeDnsName } from "@proxycore/domain";

export type CertificateResult = {
  certificatePem: string;
  privateKeyPem: string;
  secretId: string;
  expiresAt: Date;
};

export type CertificateIssueInput = {
  hostnames: string[];
  issuer: "self-signed" | "uploaded" | "letsencrypt";
  challenge: "none" | "http-01" | "dns-01";
  environment?: string;
  email?: string;
  keyType?: "rsa" | "ecdsa";
  propagationSeconds?: number;
  directoryUrl?: string;
  certificatePem?: string;
  privateKeyPem?: string;
  cloudflare?: {
    apiToken?: string;
    zoneId?: string;
    zoneName?: string;
  };
  http01?: Http01ChallengeStore;
};

export interface SecretStore {
  put(purpose: string, plaintext: string): Promise<string>;
  get(id: string): Promise<string | undefined>;
}

export class InMemorySecretStore implements SecretStore {
  private readonly values = new Map<string, string>();

  constructor(private readonly masterKeyBase64: string) {}

  async put(purpose: string, plaintext: string): Promise<string> {
    const id = `${purpose}-${randomUUID()}`;
    this.values.set(id, encryptSecret(plaintext, this.masterKeyBase64));
    return id;
  }

  async get(id: string): Promise<string | undefined> {
    const value = this.values.get(id);
    return value ? decryptSecret(value, this.masterKeyBase64) : undefined;
  }

  raw(id: string): string | undefined {
    return this.values.get(id);
  }
}

export async function issueSelfSigned(
  hostnames: string[],
  options: {
    secretStore: SecretStore;
    masterKeyBase64: string;
    validityDays?: number;
    now?: () => Date;
  },
): Promise<CertificateResult> {
  const names = normalizeHostnames(hostnames);
  if (!options.masterKeyBase64) {
    throw new Error("Master key is required for certificate issuance");
  }
  const now = options.now?.() ?? new Date();
  const expiresAt = new Date(
    now.getTime() + (options.validityDays ?? 365) * 24 * 60 * 60 * 1_000,
  );
  const pems = await selfsigned.generate(
    [{ name: "commonName", value: names[0] }],
    {
      keySize: 2_048,
      algorithm: "sha256",
      notBeforeDate: now,
      notAfterDate: expiresAt,
      extensions: [
        { name: "basicConstraints", cA: false },
        { name: "keyUsage", digitalSignature: true, keyEncipherment: true },
        {
          name: "extKeyUsage",
          serverAuth: true,
        },
        {
          name: "subjectAltName",
          altNames: names.map((name) =>
            isIP(name) > 0
              ? { type: 7 as const, ip: name }
              : { type: 2 as const, value: name },
          ),
        },
      ],
    },
  );
  const secretId = await options.secretStore.put(
    "certificate-private-key",
    pems.private,
  );
  return {
    certificatePem: pems.cert,
    privateKeyPem: pems.private,
    secretId,
    expiresAt,
  };
}

export function validateUploadedCertificate(
  hostnames: string[],
  certificatePem: string,
  privateKeyPem: string,
  now = new Date(),
): { certificatePem: string; privateKeyPem: string; expiresAt: Date } {
  const names = normalizeHostnames(hostnames);
  if (!certificatePem.includes("BEGIN CERTIFICATE")) {
    throw new Error("Certificate PEM is required");
  }
  if (!privateKeyPem.match(/BEGIN [A-Z ]*PRIVATE KEY/)) {
    throw new Error("Private key PEM is required");
  }

  const leafPem = firstCertificatePem(certificatePem);
  const certificate = new X509Certificate(leafPem);
  const expiresAt = new Date(certificate.validTo);
  if (!Number.isFinite(expiresAt.getTime())) {
    throw new Error("Certificate expiry could not be read");
  }
  if (expiresAt <= now) {
    throw new Error("Certificate is already expired");
  }

  const certificateNames = (certificate.subjectAltName ?? "")
    .split(/,\s*/)
    .map((entry) => entry.trim())
    .flatMap((entry) => {
      if (entry.startsWith("DNS:")) return [entry.slice("DNS:".length)];
      if (entry.startsWith("IP Address:")) {
        return [entry.slice("IP Address:".length)];
      }
      return [];
    });
  if (
    certificateNames.length === 0 ||
    !certificateCoversHostnames(certificateNames, names)
  ) {
    throw new Error("Certificate SANs do not cover every requested hostname");
  }

  const certificatePublicKey = Buffer.from(
    certificate.publicKey.export({
      type: "spki",
      format: "der",
    }),
  );
  const privateKeyPublicKey = Buffer.from(
    createPublicKey(createPrivateKey(privateKeyPem)).export({
      type: "spki",
      format: "der",
    }),
  );
  if (!certificatePublicKey.equals(privateKeyPublicKey)) {
    throw new Error("Certificate and private key do not match");
  }

  return {
    certificatePem: normalizePem(certificatePem),
    privateKeyPem: normalizePem(privateKeyPem),
    expiresAt,
  };
}

export function certificateCoversHostnames(
  certificateHostnames: string[],
  routeHostnames: string[],
): boolean {
  const certificates = normalizeHostnames(certificateHostnames);
  return normalizeHostnames(routeHostnames).every((hostname) =>
    certificates.some((certificateHostname) =>
      certificateHostname.startsWith("*.")
        ? hostname.endsWith(certificateHostname.slice(1)) &&
          hostname.split(".").length === certificateHostname.split(".").length
        : certificateHostname === hostname,
    ),
  );
}

export interface Dns01Adapter {
  present(hostname: string, value: string): Promise<void>;
  observe(hostname: string): Promise<string[]>;
  cleanup(hostname: string, value: string): Promise<void>;
}

export class FakeDns01Adapter implements Dns01Adapter {
  private readonly records = new Map<string, Set<string>>();

  constructor(private readonly zoneName: string) {}

  async present(hostname: string, value: string): Promise<void> {
    const name = challengeRecordName(hostname, this.zoneName);
    const values = this.records.get(name) ?? new Set<string>();
    values.add(value);
    this.records.set(name, values);
  }

  async observe(hostname: string): Promise<string[]> {
    return [
      ...(this.records.get(challengeRecordName(hostname, this.zoneName)) ??
        new Set()),
    ];
  }

  async cleanup(hostname: string, value: string): Promise<void> {
    const name = challengeRecordName(hostname, this.zoneName);
    const values = this.records.get(name);
    values?.delete(value);
    if (values && values.size === 0) this.records.delete(name);
  }
}

export class CloudflareDns01Adapter implements Dns01Adapter {
  private readonly fetchImpl: typeof fetch;
  private readonly zones = new Map<string, CloudflareZone>();

  constructor(
    private readonly options: {
      apiToken: string;
      zoneId?: string;
      zoneName?: string;
      fetchImpl?: typeof fetch;
    },
  ) {
    this.fetchImpl = options.fetchImpl ?? fetch;
  }

  async present(hostname: string, value: string): Promise<void> {
    const zone = await this.resolveZone(hostname);
    const name = challengeRecordName(hostname, zone.name);
    const response = await this.request(
      `/dns_records`,
      {
        method: "POST",
        body: JSON.stringify({ type: "TXT", name, content: value, ttl: 120 }),
      },
      zone.id,
    );
    if (!response.success)
      throw new Error("Cloudflare DNS-01 record creation failed");
  }

  async observe(hostname: string): Promise<string[]> {
    const zone = await this.resolveZone(hostname);
    const name = challengeRecordName(hostname, zone.name);
    const response = await this.request(
      `/dns_records?type=TXT&name=${encodeURIComponent(name)}`,
      { method: "GET" },
      zone.id,
    );
    return response.result
      .filter((record) => record.type === "TXT")
      .map((record) => record.content);
  }

  async cleanup(hostname: string, value: string): Promise<void> {
    const zone = await this.resolveZone(hostname);
    const name = challengeRecordName(hostname, zone.name);
    const response = await this.request(
      `/dns_records?type=TXT&name=${encodeURIComponent(name)}`,
      { method: "GET" },
      zone.id,
    );
    for (const record of response.result) {
      if (record.type === "TXT" && record.content === value) {
        await this.request(
          `/dns_records/${record.id}`,
          { method: "DELETE" },
          zone.id,
        );
      }
    }
  }

  private async resolveZone(hostname: string): Promise<CloudflareZone> {
    const normalizedHostname = normalizeDnsName(
      hostname.replace(/^\*\./, ""),
      false,
    );
    const configuredZoneId = this.options.zoneId?.trim() || undefined;
    const configuredZoneName = this.options.zoneName?.trim() || undefined;
    const cached = this.zones.get(normalizedHostname);
    if (cached) return cached;

    if (configuredZoneId && configuredZoneName) {
      const zone = {
        id: configuredZoneId,
        name: normalizeDnsName(configuredZoneName, false),
      };
      this.zones.set(normalizedHostname, zone);
      return zone;
    }

    const labels = normalizedHostname.split(".");
    for (let index = 0; index < labels.length - 1; index += 1) {
      const candidate = labels.slice(index).join(".");
      if (
        configuredZoneName &&
        candidate.toLowerCase() !== configuredZoneName.toLowerCase()
      ) {
        continue;
      }
      const response = await this.request<CloudflareZone>(
        `/zones?name=${encodeURIComponent(candidate)}&status=active&per_page=50`,
        { method: "GET" },
      );
      const zone = response.result.find(
        (item) => item.name.toLowerCase() === candidate.toLowerCase(),
      );
      if (!zone) continue;
      const resolved = {
        id: configuredZoneId ?? zone.id,
        name: zone.name,
      };
      this.zones.set(normalizedHostname, resolved);
      return resolved;
    }
    throw new Error(`Cloudflare zone could not be found for ${hostname}`);
  }

  private async request<T = CloudflareRecord>(
    path: string,
    init: RequestInit,
    zoneId?: string,
  ): Promise<CloudflareResponse<T>> {
    const base = zoneId
      ? `https://api.cloudflare.com/client/v4/zones/${zoneId}`
      : "https://api.cloudflare.com/client/v4";
    const response = await this.fetchImpl(`${base}${path}`, {
      ...init,
      headers: {
        Authorization: `Bearer ${this.options.apiToken}`,
        "Content-Type": "application/json",
        ...(init.headers ?? {}),
      },
    });
    const body = (await response.json().catch(() => undefined)) as
      CloudflareResponse<T> | undefined;
    if (!response.ok) {
      throw new Error(
        `Cloudflare DNS-01 request failed (${response.status})${formatCloudflareErrors(
          body?.errors,
        )}`,
      );
    }
    if (!body?.success) {
      throw new Error(
        `Cloudflare DNS-01 provider rejected the request${formatCloudflareErrors(
          body?.errors,
        )}`,
      );
    }
    return body;
  }
}

function formatCloudflareErrors(errors?: CloudflareApiError[]): string {
  if (!errors?.length) return "";
  const details = errors
    .map((error) =>
      error.code
        ? `[${error.code}] ${error.message ?? "Unknown provider error"}`
        : (error.message ?? "Unknown provider error"),
    )
    .join("; ");
  return `: ${details}`;
}

export type Http01ChallengeStore = {
  put(token: string, keyAuthorization: string, expiresAt?: Date): void;
  get(token: string): string | undefined;
  remove(token: string): void;
};

export class InMemoryHttp01ChallengeStore implements Http01ChallengeStore {
  private readonly values = new Map<
    string,
    { keyAuthorization: string; expiresAt?: Date }
  >();

  put(token: string, keyAuthorization: string, expiresAt?: Date): void {
    this.values.set(token, { keyAuthorization, expiresAt });
  }

  get(token: string): string | undefined {
    const value = this.values.get(token);
    if (value?.expiresAt && value.expiresAt <= new Date()) {
      this.values.delete(token);
      return undefined;
    }
    return value?.keyAuthorization;
  }

  remove(token: string): void {
    this.values.delete(token);
  }
}

export async function issueWithAcme(options: {
  hostnames: string[];
  directoryUrl: string;
  accountKeyPem: string;
  email?: string;
  challenge: "http-01" | "dns-01";
  keyType?: "rsa" | "ecdsa";
  propagationSeconds?: number;
  http01?: Http01ChallengeStore;
  dns01?: Dns01Adapter;
}): Promise<{
  certificatePem: string;
  privateKeyPem: string;
  expiresAt: Date;
}> {
  const names = normalizeHostnames(options.hostnames);
  if (
    options.challenge === "http-01" &&
    names.some((hostname) => hostname.startsWith("*."))
  ) {
    throw new Error("Let's Encrypt HTTP-01 cannot issue wildcard certificates");
  }
  if (options.challenge === "http-01" && !options.http01) {
    throw new Error("HTTP-01 challenge store is required");
  }
  if (options.challenge === "dns-01" && !options.dns01) {
    throw new Error("DNS-01 adapter is required");
  }
  const certificateKey =
    options.keyType === "ecdsa"
      ? await acme.crypto.createPrivateEcdsaKey("P-256")
      : await acme.crypto.createPrivateKey();
  const [privateKey, csr] = await acme.crypto.createCsr(
    {
      commonName: names[0],
      altNames: names,
    },
    certificateKey,
  );
  const client = new acme.Client({
    directoryUrl: options.directoryUrl,
    accountKey: options.accountKeyPem,
  });
  const certificatePem = await client.auto({
    csr,
    email: options.email,
    termsOfServiceAgreed: true,
    challengePriority: [options.challenge],
    challengeCreateFn: async (authz, challenge, keyAuthorization) => {
      if (challenge.type === "http-01") {
        options.http01?.put(challenge.token, keyAuthorization);
        return;
      }
      await options.dns01?.present(authz.identifier.value, keyAuthorization);
      await waitSeconds(options.propagationSeconds);
    },
    challengeRemoveFn: async (authz, challenge, keyAuthorization) => {
      if (challenge.type === "http-01") {
        options.http01?.remove(challenge.token);
        return;
      }
      await options.dns01?.cleanup(authz.identifier.value, keyAuthorization);
    },
  });
  const expiresAt = new X509Certificate(certificatePem).validTo
    ? new Date(new X509Certificate(certificatePem).validTo)
    : new Date(Date.now() + 60 * 24 * 60 * 60 * 1_000);
  return {
    certificatePem,
    privateKeyPem: privateKey.toString(),
    expiresAt,
  };
}

export async function issueLetsEncrypt(options: {
  hostnames: string[];
  directoryUrl: string;
  email?: string;
  challenge: "http-01" | "dns-01";
  keyType?: "rsa" | "ecdsa";
  propagationSeconds?: number;
  http01?: Http01ChallengeStore;
  dns01?: Dns01Adapter;
}): Promise<{
  certificatePem: string;
  privateKeyPem: string;
  expiresAt: Date;
}> {
  const accountKeyPem = (await acme.crypto.createPrivateKey()).toString();
  return issueWithAcme({
    ...options,
    accountKeyPem,
  });
}

async function waitSeconds(seconds = 0): Promise<void> {
  const milliseconds = Math.max(0, Math.min(seconds, 600)) * 1_000;
  if (milliseconds === 0) return;
  await new Promise((resolve) => setTimeout(resolve, milliseconds));
}

function normalizeHostnames(hostnames: string[]): string[] {
  if (hostnames.length === 0) {
    throw new Error("At least one certificate hostname is required");
  }
  return [
    ...new Set(
      hostnames.map((hostname) => {
        const normalized = hostname.trim().replace(/^\[|\]$/g, "");
        return isIP(normalized) > 0
          ? normalized.toLowerCase()
          : normalizeDnsName(normalized);
      }),
    ),
  ];
}

function firstCertificatePem(certificatePem: string): string {
  const match = certificatePem.match(
    /-----BEGIN CERTIFICATE-----[\s\S]+?-----END CERTIFICATE-----/,
  );
  if (!match) throw new Error("Certificate PEM is invalid");
  return `${match[0]}\n`;
}

function normalizePem(value: string): string {
  return `${value.trim()}\n`;
}

type CloudflareZone = {
  id: string;
  name: string;
};

type CloudflareRecord = {
  id: string;
  type: string;
  content: string;
};

type CloudflareResponse<T = CloudflareRecord> = {
  success: boolean;
  result: T[];
  errors?: CloudflareApiError[];
  messages?: CloudflareApiError[];
};

type CloudflareApiError = {
  code?: number;
  message?: string;
};

function challengeRecordName(hostname: string, zoneName: string): string {
  const raw = hostname.trim().toLowerCase().replace(/\.+$/, "");
  const hostWithoutPrefix = raw.startsWith("_acme-challenge.")
    ? raw.slice("_acme-challenge.".length)
    : raw;
  const normalizedHost = normalizeDnsName(hostWithoutPrefix, false);
  const zone = normalizeDnsName(zoneName, false);
  if (normalizedHost !== zone && !normalizedHost.endsWith(`.${zone}`)) {
    throw new Error(
      "DNS-01 adapter only accepts _acme-challenge records in its zone",
    );
  }
  return `_acme-challenge.${normalizedHost}`;
}

export * from "./basic-auth";
export * from "./materialize";
