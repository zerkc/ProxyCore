import type { CertificateIssueInput } from "@proxycore/certificates";
import type { CertificateStatus } from "@proxycore/domain";
import { http01ChallengeStore } from "../../../server/acme";
import { getWebContext } from "../../../server/context";
import {
  apiError,
  HttpError,
  readJson,
  requireUser,
} from "../../../server/http";

export async function GET(request: Request) {
  try {
    await requireUser(request);
    return Response.json({
      certificates: (
        await getWebContext().configuration.listCertificates()
      ).map(toPublicCertificate),
    });
  } catch (error) {
    return apiError(error);
  }
}

export async function POST(request: Request) {
  try {
    const { user } = await requireUser(request);
    const context = getWebContext();
    const input = request.headers
      .get("content-type")
      ?.startsWith("multipart/form-data")
      ? await parseMultipartRequest(request, context.config)
      : parseJsonRequest(await readJson(request), context.config);
    const certificate = await context.configuration.issueCertificate(
      input,
      user.id,
    );
    const publicCertificate = toPublicCertificate(certificate);
    if (certificate.status === "failed") {
      return Response.json(
        {
          certificate: publicCertificate,
          error: certificate.failureReason ?? "Certificate issuance failed",
        },
        { status: 422 },
      );
    }
    return Response.json({ certificate: publicCertificate }, { status: 201 });
  } catch (error) {
    return apiError(error);
  }
}

async function parseMultipartRequest(
  request: Request,
  config: ReturnType<typeof getWebContext>["config"],
): Promise<CertificateIssueInput> {
  const form = await request.formData();
  const issuer = stringField(form, "issuer");
  const challenge = stringField(form, "challenge");
  const hostnames = parseHostnames(form.get("hostnames"));
  const certificatePem = await fileField(form, "certificate");
  const privateKeyPem = await fileField(form, "privateKey");
  return buildInput(
    {
      hostnames,
      issuer,
      challenge,
      environment: stringField(form, "environment"),
      email: stringField(form, "email"),
      keyType: stringField(form, "keyType"),
      propagationSeconds: stringField(form, "propagationSeconds"),
      certificatePem,
      privateKeyPem,
      cloudflare: {
        apiToken: stringField(form, "cloudflareApiToken"),
        zoneId: stringField(form, "cloudflareZoneId"),
        zoneName: stringField(form, "cloudflareZoneName"),
      },
    },
    config,
  );
}

function parseJsonRequest(
  body: Record<string, unknown>,
  config: ReturnType<typeof getWebContext>["config"],
): CertificateIssueInput {
  const cloudflare =
    body.cloudflare && typeof body.cloudflare === "object"
      ? (body.cloudflare as Record<string, unknown>)
      : {};
  return buildInput(
    {
      hostnames: body.hostnames,
      issuer: body.issuer,
      challenge: body.challenge,
      environment: body.environment,
      email: body.email,
      keyType: body.keyType,
      propagationSeconds: body.propagationSeconds,
      certificatePem: body.certificatePem,
      privateKeyPem: body.privateKeyPem,
      cloudflare: {
        apiToken: cloudflare.apiToken,
        zoneId: cloudflare.zoneId,
        zoneName: cloudflare.zoneName,
      },
    },
    config,
  );
}

function buildInput(
  raw: {
    hostnames: unknown;
    issuer: unknown;
    challenge: unknown;
    environment?: unknown;
    email?: unknown;
    keyType?: unknown;
    propagationSeconds?: unknown;
    certificatePem?: unknown;
    privateKeyPem?: unknown;
    cloudflare?: {
      apiToken?: unknown;
      zoneId?: unknown;
      zoneName?: unknown;
    };
  },
  config: ReturnType<typeof getWebContext>["config"],
): CertificateIssueInput {
  if (
    !Array.isArray(raw.hostnames) ||
    raw.hostnames.length === 0 ||
    raw.hostnames.length > 100 ||
    raw.hostnames.some(
      (hostname) =>
        typeof hostname !== "string" || hostname.trim().length === 0,
    )
  ) {
    throw new HttpError(400, "Enter at least one valid certificate hostname");
  }
  if (
    raw.issuer !== "self-signed" &&
    raw.issuer !== "uploaded" &&
    raw.issuer !== "letsencrypt"
  ) {
    throw new HttpError(400, "Invalid certificate source");
  }
  if (
    raw.challenge !== "none" &&
    raw.challenge !== "http-01" &&
    raw.challenge !== "dns-01"
  ) {
    throw new HttpError(400, "Invalid ACME challenge");
  }
  const issuer = raw.issuer as CertificateIssueInput["issuer"];
  const challenge = raw.challenge as CertificateIssueInput["challenge"];
  const environment =
    typeof raw.environment === "string" && raw.environment.trim()
      ? raw.environment.trim()
      : issuer === "letsencrypt"
        ? "staging"
        : "local";
  if (!["local", "staging", "production"].includes(environment)) {
    throw new HttpError(400, "Invalid certificate environment");
  }
  if (issuer !== "letsencrypt" && challenge !== "none") {
    throw new HttpError(
      400,
      "Self-signed and uploaded certificates use no challenge",
    );
  }
  if (issuer === "letsencrypt" && challenge === "none") {
    throw new HttpError(400, "Let's Encrypt requires HTTP-01 or DNS-01");
  }
  const keyType =
    raw.keyType === undefined || raw.keyType === null || raw.keyType === ""
      ? "rsa"
      : raw.keyType;
  if (keyType !== "rsa" && keyType !== "ecdsa") {
    throw new HttpError(400, "Invalid certificate key type");
  }
  const propagationSeconds =
    challenge === "dns-01"
      ? raw.propagationSeconds === undefined ||
        raw.propagationSeconds === null ||
        raw.propagationSeconds === ""
        ? 30
        : Number(raw.propagationSeconds)
      : undefined;
  if (
    propagationSeconds !== undefined &&
    (!Number.isInteger(propagationSeconds) ||
      propagationSeconds < 0 ||
      propagationSeconds > 600)
  ) {
    throw new HttpError(400, "Propagation seconds must be between 0 and 600");
  }

  const certificatePem = stringValue(raw.certificatePem);
  const privateKeyPem = stringValue(raw.privateKeyPem);
  if (issuer === "uploaded" && (!certificatePem || !privateKeyPem)) {
    throw new HttpError(
      400,
      "Upload both the certificate PEM and the private key PEM",
    );
  }

  const cloudflare =
    issuer === "letsencrypt" && challenge === "dns-01"
      ? {
          apiToken:
            stringValue(raw.cloudflare?.apiToken) ??
            stringValue(config.cloudflare.apiToken),
          zoneId:
            stringValue(raw.cloudflare?.zoneId) ??
            stringValue(config.cloudflare.zoneId),
          zoneName: stringValue(raw.cloudflare?.zoneName),
        }
      : undefined;
  if (
    cloudflare &&
    !cloudflare.apiToken &&
    !config.cloudflare.apiToken &&
    config.persistenceMode === "memory"
  ) {
    throw new HttpError(400, "DNS-01 requires a Cloudflare API token");
  }

  return {
    hostnames: raw.hostnames.map((hostname) => String(hostname).trim()),
    issuer,
    challenge,
    environment,
    email: stringValue(raw.email),
    keyType,
    propagationSeconds,
    directoryUrl:
      issuer === "letsencrypt"
        ? environment === "production"
          ? config.acmeProductionDirectoryUrl
          : config.acmeDirectoryUrl
        : undefined,
    certificatePem,
    privateKeyPem,
    cloudflare,
    http01: challenge === "http-01" ? http01ChallengeStore : undefined,
  };
}

function parseHostnames(value: FormDataEntryValue | null): string[] {
  if (typeof value !== "string") {
    throw new HttpError(400, "Hostnames are required");
  }
  return value
    .split(/[,\n]/)
    .map((hostname) => hostname.trim())
    .filter(Boolean);
}

async function fileField(
  form: FormData,
  name: string,
): Promise<string | undefined> {
  const value = form.get(name);
  if (!value) return undefined;
  if (typeof value === "string") return value;
  if (value.size > 2 * 1024 * 1024) {
    throw new HttpError(413, `${name} is larger than 2 MiB`);
  }
  return value.text();
}

function stringField(form: FormData, name: string): string | undefined {
  const value = form.get(name);
  return typeof value === "string" && value.trim() ? value.trim() : undefined;
}

function stringValue(value: unknown): string | undefined {
  return typeof value === "string" && value.trim() ? value.trim() : undefined;
}

function toPublicCertificate(certificate: CertificateStatus) {
  const {
    secretId: _secretId,
    certificatePem: _certificatePem,
    ...publicCertificate
  } = certificate;
  return publicCertificate;
}
