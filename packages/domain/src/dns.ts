import { isIP } from "node:net";
import { assertDomain } from "./errors";
import type {
  DnsRecord,
  DnsRecordInput,
  ForwardingRule,
  IngressAddresses,
  MxValue,
  RecordValue,
  ResolverEndpoint,
  ResolverPool,
  SrvValue,
  UpstreamTarget,
} from "./model";

export const MIN_TTL = 30;
export const MAX_TTL = 86_400;
export const DEFAULT_TTL = 300;

const recordTypes = new Set(["A", "AAAA", "CNAME", "TXT", "MX", "SRV"]);
const dnsLabelPattern = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/;

export function normalizeDnsName(value: string, allowWildcard = true): string {
  const withoutRoot = value.trim().replace(/\.+$/, "").toLowerCase();
  assertDomain(withoutRoot.length > 0, "DNS name cannot be empty", "DNS_NAME_EMPTY");
  const labels = withoutRoot.split(".");
  assertDomain(labels.length <= 127, "DNS name has too many labels", "DNS_NAME_LABELS");

  labels.forEach((label, index) => {
    if (allowWildcard && index === 0 && label === "*") {
      return;
    }
    assertDomain(
      label.length <= 63 && dnsLabelPattern.test(label),
      `Invalid DNS label: ${label}`,
      "DNS_LABEL_INVALID",
    );
  });

  assertDomain(withoutRoot.length <= 253, "DNS name is too long", "DNS_NAME_LENGTH");
  return withoutRoot;
}

export function canonicalRecordName(name: string, zoneName: string): string {
  const zone = normalizeDnsName(zoneName, false);
  const value = name.trim();
  if (value === "" || value === "@") {
    return zone;
  }

  const normalized = normalizeDnsName(value);
  if (normalized === zone || normalized.endsWith(`.${zone}`)) {
    return normalized;
  }
  return `${normalized}.${zone}`;
}

export function validateRecordSet(
  records: DnsRecordInput[],
  options: { zoneName: string; ingress?: IngressAddresses },
): DnsRecord[] {
  const zoneName = normalizeDnsName(options.zoneName, false);
  const seenIds = new Set<string>();
  const normalized = records.map((record) => {
    assertDomain(record.id.trim().length > 0, "Record id is required", "RECORD_ID_REQUIRED");
    assertDomain(!seenIds.has(record.id), `Duplicate record id: ${record.id}`, "RECORD_ID_DUPLICATE");
    seenIds.add(record.id);
    assertDomain(recordTypes.has(record.type), `Unsupported record type: ${record.type}`, "RECORD_TYPE");

    const name = canonicalRecordName(record.name, zoneName);
    const ttl = record.ttl ?? DEFAULT_TTL;
    assertDomain(
      Number.isInteger(ttl) && ttl >= MIN_TTL && ttl <= MAX_TTL,
      `TTL must be between ${MIN_TTL} and ${MAX_TTL}`,
      "TTL_INVALID",
    );
    validateRecordValue(record.type, record.value, name);

    if (record.proxied) {
      assertDomain(
        record.type === "A" || record.type === "AAAA" || record.type === "CNAME",
        `${record.type} records cannot be proxied`,
        "PROXY_RECORD_TYPE",
      );
      assertDomain(record.proxy, "Proxied records require proxy settings", "PROXY_SETTINGS_REQUIRED");
      validateIngressForRecord(record.type, options.ingress);
      validateProxyOrigin(record.proxy.origin);
    } else if (record.proxy) {
      validateProxyOrigin(record.proxy.origin);
    }

    return { ...record, name, ttl };
  });

  const enabledByName = new Map<string, DnsRecord[]>();
  normalized
    .filter((record) => record.enabled)
    .forEach((record) => {
      const group = enabledByName.get(record.name) ?? [];
      group.push(record);
      enabledByName.set(record.name, group);
    });

  for (const [name, group] of enabledByName) {
    const cname = group.find((record) => record.type === "CNAME");
    if (cname && group.length > 1) {
      throw new Error(`CNAME cannot coexist with other records for ${name}`);
    }
    const proxied = group.filter((record) => record.proxied);
    assertDomain(
      proxied.length <= 1,
      `Only one record may be proxied for ${name}`,
      "PROXY_HOST_CONFLICT",
    );
  }

  return normalized;
}

function validateRecordValue(type: DnsRecordInput["type"], value: RecordValue, name: string): void {
  switch (type) {
    case "A":
      assertDomain(typeof value === "string" && isIP(value) === 4, `Invalid A value for ${name}`, "A_VALUE");
      return;
    case "AAAA":
      assertDomain(typeof value === "string" && isIP(value) === 6, `Invalid AAAA value for ${name}`, "AAAA_VALUE");
      return;
    case "CNAME":
      assertDomain(typeof value === "string", `Invalid CNAME value for ${name}`, "CNAME_VALUE");
      normalizeDnsName(value);
      return;
    case "TXT":
      assertDomain(
        typeof value === "string" && value.length > 0 && value.length <= 255 && !/[\r\n]/.test(value),
        `Invalid TXT value for ${name}`,
        "TXT_VALUE",
      );
      return;
    case "MX":
      assertMxValue(value, name);
      return;
    case "SRV":
      assertSrvValue(value, name);
      return;
  }
}

function assertMxValue(value: RecordValue, name: string): asserts value is MxValue {
  assertDomain(
    typeof value === "object" &&
      value !== null &&
      "priority" in value &&
      "exchange" in value &&
      Number.isInteger(value.priority) &&
      value.priority >= 0 &&
      value.priority <= 65_535 &&
      typeof value.exchange === "string",
    `Invalid MX value for ${name}`,
    "MX_VALUE",
  );
  normalizeDnsName(value.exchange);
}

function assertSrvValue(value: RecordValue, name: string): asserts value is SrvValue {
  assertDomain(
    typeof value === "object" &&
      value !== null &&
      "priority" in value &&
      "weight" in value &&
      "port" in value &&
      "target" in value &&
      Number.isInteger(value.priority) &&
      value.priority >= 0 &&
      value.priority <= 65_535 &&
      Number.isInteger(value.weight) &&
      value.weight >= 0 &&
      value.weight <= 65_535 &&
      Number.isInteger(value.port) &&
      value.port >= 0 &&
      value.port <= 65_535 &&
      typeof value.target === "string",
    `Invalid SRV value for ${name}`,
    "SRV_VALUE",
  );
  normalizeDnsName(value.target);
}

function validateIngressForRecord(
  type: DnsRecordInput["type"],
  ingress: IngressAddresses | undefined,
): void {
  assertDomain(ingress, "Proxy ingress addresses are required", "INGRESS_REQUIRED");
  if (type === "A") {
    assertDomain(isIP(ingress.ipv4 ?? "") === 4, "IPv4 proxy ingress is required", "INGRESS_IPV4_REQUIRED");
  } else if (type === "AAAA") {
    assertDomain(isIP(ingress.ipv6 ?? "") === 6, "IPv6 proxy ingress is required", "INGRESS_IPV6_REQUIRED");
  } else {
    assertDomain(
      isIP(ingress.ipv4 ?? "") === 4 || isIP(ingress.ipv6 ?? "") === 6,
      "At least one proxy ingress address is required",
      "INGRESS_REQUIRED",
    );
  }
}

function validateProxyOrigin(origin: UpstreamTarget): void {
  assertDomain(origin && typeof origin === "object", "Proxy origin is required", "ORIGIN_REQUIRED");
  assertDomain(isIP(origin.ip) > 0, "Proxy origin must be a literal IP", "ORIGIN_IP");
  assertDomain(
    Number.isInteger(origin.port) && origin.port >= 1 && origin.port <= 65_535,
    "Proxy origin port is invalid",
    "ORIGIN_PORT",
  );
  assertDomain(
    origin.protocol === "http" || origin.protocol === "https",
    "Proxy origin protocol must be http or https",
    "ORIGIN_PROTOCOL",
  );
}

export function validateResolverPool(pool: ResolverPool): ResolverPool {
  assertDomain(pool.id.trim().length > 0, "Resolver pool id is required", "POOL_ID");
  assertDomain(pool.endpoints.length > 0, "Resolver pool needs an endpoint", "POOL_EMPTY");
  return {
    ...pool,
    endpoints: pool.endpoints.map(validateResolverEndpoint),
  };
}

function validateResolverEndpoint(endpoint: ResolverEndpoint): ResolverEndpoint {
  assertDomain(isIP(endpoint.host) > 0, "Resolver endpoint must be a literal IP", "RESOLVER_IP");
  assertDomain(
    Number.isInteger(endpoint.port) && endpoint.port >= 1 && endpoint.port <= 65_535,
    "Resolver endpoint port is invalid",
    "RESOLVER_PORT",
  );
  return endpoint;
}

export function validateForwardingRules(rules: ForwardingRule[]): ForwardingRule[] {
  const seen = new Set<string>();
  return rules.map((rule) => {
    const suffix = normalizeDnsName(rule.suffix, false);
    assertDomain(!seen.has(suffix), `Duplicate forwarding suffix: ${suffix}`, "FORWARD_SUFFIX_DUPLICATE");
    seen.add(suffix);
    return { suffix, pool: validateResolverPool(rule.pool) };
  });
}

export function selectForwardingPool(
  queryName: string,
  rules: ForwardingRule[],
): ResolverPool | undefined {
  const normalized = normalizeDnsName(queryName);
  return validateForwardingRules(rules)
    .filter(
      (rule) => normalized === rule.suffix || normalized.endsWith(`.${rule.suffix}`),
    )
    .sort((left, right) => right.suffix.split(".").length - left.suffix.split(".").length)[0]?.pool;
}
