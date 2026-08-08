import { createHash, randomUUID, X509Certificate } from "node:crypto";
import * as acme from "acme-client";
import * as selfsigned from "selfsigned";
import {
  decryptSecret,
  encryptSecret,
} from "@proxycore/crypto";
import { normalizeDnsName } from "@proxycore/domain";

export type CertificateResult = {
  certificatePem: string;
  privateKeyPem: string;
  secretId: string;
  expiresAt: Date;
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
            /^[0-9a-f:.]+$/i.test(name)
              ? { type: 7 as const, ip: name }
              : { type: 2 as const, value: name },
          ),
        },
      ],
    },
  );
  const secretId = await options.secretStore.put("certificate-private-key", pems.private);
  return {
    certificatePem: pems.cert,
    privateKeyPem: pems.private,
    secretId,
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
    return [...(this.records.get(challengeRecordName(hostname, this.zoneName)) ?? new Set())];
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

  constructor(
    private readonly options: {
      apiToken: string;
      zoneId: string;
      zoneName: string;
      fetchImpl?: typeof fetch;
    },
  ) {
    this.fetchImpl = options.fetchImpl ?? fetch;
  }

  async present(hostname: string, value: string): Promise<void> {
    const name = challengeRecordName(hostname, this.options.zoneName);
    const response = await this.request(`/dns_records`, {
      method: "POST",
      body: JSON.stringify({ type: "TXT", name, content: value, ttl: 120 }),
    });
    if (!response.success) throw new Error("Cloudflare DNS-01 record creation failed");
  }

  async observe(hostname: string): Promise<string[]> {
    const name = challengeRecordName(hostname, this.options.zoneName);
    const response = await this.request(
      `/dns_records?type=TXT&name=${encodeURIComponent(name)}`,
      { method: "GET" },
    );
    return response.result
      .filter((record) => record.type === "TXT")
      .map((record) => record.content);
  }

  async cleanup(hostname: string, value: string): Promise<void> {
    const name = challengeRecordName(hostname, this.options.zoneName);
    const response = await this.request(
      `/dns_records?type=TXT&name=${encodeURIComponent(name)}`,
      { method: "GET" },
    );
    for (const record of response.result) {
      if (record.type === "TXT" && record.content === value) {
        await this.request(`/dns_records/${record.id}`, { method: "DELETE" });
      }
    }
  }

  private async request(
    path: string,
    init: RequestInit,
  ): Promise<CloudflareResponse> {
    const response = await this.fetchImpl(
      `https://api.cloudflare.com/client/v4/zones/${this.options.zoneId}${path}`,
      {
        ...init,
        headers: {
          Authorization: `Bearer ${this.options.apiToken}`,
          "Content-Type": "application/json",
          ...(init.headers ?? {}),
        },
      },
    );
    if (!response.ok) {
      throw new Error(`Cloudflare DNS-01 request failed (${response.status})`);
    }
    const body = (await response.json()) as CloudflareResponse;
    if (!body.success) throw new Error("Cloudflare DNS-01 provider rejected the request");
    return body;
  }

}

export type Http01ChallengeStore = {
  put(token: string, keyAuthorization: string, expiresAt?: Date): void;
  get(token: string): string | undefined;
  remove(token: string): void;
};

export class InMemoryHttp01ChallengeStore implements Http01ChallengeStore {
  private readonly values = new Map<string, { keyAuthorization: string; expiresAt?: Date }>();

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
  http01?: Http01ChallengeStore;
  dns01?: Dns01Adapter;
}): Promise<{ certificatePem: string; privateKeyPem: string; expiresAt: Date }> {
  const names = normalizeHostnames(options.hostnames);
  if (options.challenge === "http-01" && !options.http01) {
    throw new Error("HTTP-01 challenge store is required");
  }
  if (options.challenge === "dns-01" && !options.dns01) {
    throw new Error("DNS-01 adapter is required");
  }
  const [privateKey, csr] = await acme.crypto.createCsr({
    commonName: names[0],
    altNames: names,
  });
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
      const dnsValue = createHash("sha256").update(keyAuthorization).digest("base64url");
      await options.dns01?.present(authz.identifier.value, dnsValue);
    },
    challengeRemoveFn: async (authz, challenge, keyAuthorization) => {
      if (challenge.type === "http-01") {
        options.http01?.remove(challenge.token);
        return;
      }
      const dnsValue = createHash("sha256").update(keyAuthorization).digest("base64url");
      await options.dns01?.cleanup(authz.identifier.value, dnsValue);
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

function normalizeHostnames(hostnames: string[]): string[] {
  if (hostnames.length === 0) {
    throw new Error("At least one certificate hostname is required");
  }
  return [...new Set(hostnames.map((hostname) => normalizeDnsName(hostname)))];
}

type CloudflareResponse = {
  success: boolean;
  result: Array<{ id: string; type: string; content: string }>;
};

function challengeRecordName(hostname: string, zoneName: string): string {
  const raw = hostname.trim().toLowerCase().replace(/\.+$/, "");
  const hostWithoutPrefix = raw.startsWith("_acme-challenge.")
    ? raw.slice("_acme-challenge.".length)
    : raw;
  const normalizedHost = normalizeDnsName(hostWithoutPrefix, false);
  const zone = normalizeDnsName(zoneName, false);
  if (normalizedHost !== zone && !normalizedHost.endsWith(`.${zone}`)) {
    throw new Error("DNS-01 adapter only accepts _acme-challenge records in its zone");
  }
  return `_acme-challenge.${normalizedHost}`;
}
