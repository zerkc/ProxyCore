import {
  MVP_RECORD_TYPES,
  type DnsRecordInput,
  type ProxySettings,
} from "@proxycore/domain";
import { HttpError } from "./http";

export type ProxyBasicAuthInput = {
  username: string;
  password?: string;
  passwordSecretId?: string;
};

export type ProxySettingsInput = Omit<ProxySettings, "basicAuth"> & {
  basicAuth?: ProxyBasicAuthInput;
};

export type RecordMutationInput = Omit<DnsRecordInput, "id" | "proxy"> & {
  id?: string;
  proxy?: ProxySettingsInput;
};

export function parseRecordMutationBody(
  body: Record<string, unknown>,
): RecordMutationInput {
  if (
    typeof body.name !== "string" ||
    typeof body.type !== "string" ||
    !MVP_RECORD_TYPES.includes(body.type as (typeof MVP_RECORD_TYPES)[number])
  ) {
    throw new HttpError(400, "name and a supported record type are required");
  }

  return {
    id: typeof body.id === "string" ? body.id : undefined,
    name: body.name,
    type: body.type as DnsRecordInput["type"],
    value: body.value as DnsRecordInput["value"],
    ttl: typeof body.ttl === "number" ? body.ttl : undefined,
    enabled: typeof body.enabled === "boolean" ? body.enabled : true,
    comment: typeof body.comment === "string" ? body.comment : undefined,
    proxied: typeof body.proxied === "boolean" ? body.proxied : false,
    proxy:
      body.proxy === undefined
        ? undefined
        : parseProxySettingsInput(body.proxy),
  };
}

function parseProxySettingsInput(value: unknown): ProxySettingsInput {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new HttpError(400, "proxy settings must be an object");
  }
  const proxy = value as Record<string, unknown>;
  const origin = proxy.origin;
  if (!origin || typeof origin !== "object" || Array.isArray(origin)) {
    throw new HttpError(400, "proxy origin is required");
  }
  const originRecord = origin as Record<string, unknown>;
  if (
    typeof originRecord.ip !== "string" ||
    typeof originRecord.port !== "number" ||
    typeof originRecord.protocol !== "string"
  ) {
    throw new HttpError(400, "proxy origin requires ip, port, and protocol");
  }
  if (typeof proxy.tlsEnabled !== "boolean") {
    throw new HttpError(400, "proxy tlsEnabled is required");
  }

  return {
    origin: {
      ip: originRecord.ip,
      port: originRecord.port,
      protocol: originRecord.protocol as ProxySettings["origin"]["protocol"],
    },
    tlsEnabled: proxy.tlsEnabled,
    redirectHttpToHttps:
      typeof proxy.redirectHttpToHttps === "boolean"
        ? proxy.redirectHttpToHttps
        : undefined,
    certificateId:
      typeof proxy.certificateId === "string" ? proxy.certificateId : undefined,
    http2: typeof proxy.http2 === "boolean" ? proxy.http2 : undefined,
    http3: typeof proxy.http3 === "boolean" ? proxy.http3 : undefined,
    nginxDirectives:
      typeof proxy.nginxDirectives === "string"
        ? proxy.nginxDirectives
        : undefined,
    headers: Array.isArray(proxy.headers)
      ? (proxy.headers as ProxySettings["headers"])
      : undefined,
    pathRules: Array.isArray(proxy.pathRules)
      ? (proxy.pathRules as ProxySettings["pathRules"])
      : undefined,
    basicAuth: parseBasicAuthInput(proxy.basicAuth),
    websocket:
      typeof proxy.websocket === "boolean" ? proxy.websocket : undefined,
    cache:
      proxy.cache &&
      typeof proxy.cache === "object" &&
      !Array.isArray(proxy.cache)
        ? { enabled: Boolean((proxy.cache as { enabled?: unknown }).enabled) }
        : undefined,
    backendTlsVerify:
      typeof proxy.backendTlsVerify === "boolean"
        ? proxy.backendTlsVerify
        : undefined,
    timeouts:
      proxy.timeouts &&
      typeof proxy.timeouts === "object" &&
      !Array.isArray(proxy.timeouts)
        ? (proxy.timeouts as ProxySettings["timeouts"])
        : undefined,
  };
}

function parseBasicAuthInput(value: unknown): ProxyBasicAuthInput | undefined {
  if (value === undefined || value === null) return undefined;
  if (typeof value !== "object" || Array.isArray(value)) {
    throw new HttpError(400, "basicAuth must be an object");
  }
  const auth = value as Record<string, unknown>;
  if (typeof auth.username !== "string") {
    throw new HttpError(400, "basicAuth username is required");
  }
  return {
    username: auth.username,
    password: typeof auth.password === "string" ? auth.password : undefined,
    passwordSecretId:
      typeof auth.passwordSecretId === "string"
        ? auth.passwordSecretId
        : undefined,
  };
}
