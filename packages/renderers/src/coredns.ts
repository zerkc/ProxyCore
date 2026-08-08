import { createHash } from "node:crypto";
import {
  normalizeDnsName,
  validateForwardingRules,
  validateRecordSet,
  validateResolverPool,
  type DnsRecord,
  type ForwardingRule,
  type IngressAddresses,
  type MxValue,
  type ResolverPool,
  type SrvValue,
} from "@proxycore/domain";

export type CoreDnsZoneInput = {
  name: string;
  records: DnsRecord[];
};

export type CoreDnsRenderInput = {
  zones: CoreDnsZoneInput[];
  ingress: IngressAddresses;
  defaultPool: ResolverPool;
  forwardingRules: ForwardingRule[];
};

export type CoreDnsCandidate = {
  corefile: string;
  files: Record<string, string>;
  checksum: string;
};

export function renderCoreDnsCandidate(input: CoreDnsRenderInput): CoreDnsCandidate {
  const defaultPool = validateResolverPool(input.defaultPool);
  const rules = validateForwardingRules(input.forwardingRules);
  const files: Record<string, string> = {};
  const zoneBlocks: string[] = [];

  for (const zone of [...input.zones].sort((left, right) => left.name.localeCompare(right.name))) {
    const normalizedZone = normalizeDnsName(zone.name, false);
    const records = validateRecordSet(zone.records, {
      zoneName: normalizedZone,
      ingress: input.ingress,
    });
    const relativePath = `zones/${normalizedZone}.zone`;
    files[relativePath] = renderZoneFile(normalizedZone, records, input.ingress);
    zoneBlocks.push(`${normalizedZone}:53 {\n    file ${relativePath}\n}`);
  }

  const forwardingBlocks = rules
    .sort((left, right) => right.suffix.length - left.suffix.length)
    .map(
      (rule) => `${rule.suffix}:53 {\n${renderForwardBlock(rule.pool)}\n}`,
    );
  const rootBlock = `.:53 {\n    errors\n    health\n    ready\n    loop\n    reload\n    cache 30\n${renderForwardBlock(defaultPool)}\n}`;
  const corefile = [...zoneBlocks, ...forwardingBlocks, rootBlock].join("\n\n") + "\n";
  const checksum = createHash("sha256")
    .update(corefile)
    .update(
      Object.keys(files)
        .sort()
        .map((key) => `${key}\n${files[key]}`)
        .join("\n"),
    )
    .digest("hex");

  return { corefile, files, checksum };
}

function renderZoneFile(zone: string, records: DnsRecord[], ingress: IngressAddresses): string {
  const body = [
    "$ORIGIN " + `${zone}.`,
    "$TTL 300",
    `@ 300 IN SOA ns1.${zone}. hostmaster.${zone}. 1 60 300 3600 60`,
    `@ 300 IN NS ns1.${zone}.`,
  ];

  for (const record of records
    .filter((item) => item.enabled)
    .sort((left, right) => {
      const byName = left.name.localeCompare(right.name);
      if (byName !== 0) return byName;
      const byType = left.type.localeCompare(right.type);
      if (byType !== 0) return byType;
      return JSON.stringify(left.value).localeCompare(JSON.stringify(right.value));
    })) {
    const owner = ownerName(record.name, zone);
    if (record.proxied) {
      for (const line of proxiedAnswers(owner, record, ingress)) {
        body.push(line);
      }
      continue;
    }
    body.push(`${owner} ${record.ttl} IN ${record.type} ${renderValue(record)}`);
  }

  return body.join("\n") + "\n";
}

function proxiedAnswers(owner: string, record: DnsRecord, ingress: IngressAddresses): string[] {
  const lines: string[] = [];
  if (record.type === "A" || record.type === "CNAME") {
    if (ingress.ipv4) {
      lines.push(`${owner} ${record.ttl} IN A ${ingress.ipv4}`);
    }
  }
  if (record.type === "AAAA" || record.type === "CNAME") {
    if (ingress.ipv6) {
      lines.push(`${owner} ${record.ttl} IN AAAA ${ingress.ipv6}`);
    }
  }
  return lines;
}

function renderValue(record: DnsRecord): string {
  switch (record.type) {
    case "TXT":
      return `"${String(record.value).replaceAll("\\", "\\\\").replaceAll('"', '\\"')}"`;
    case "MX":
      return `${(record.value as MxValue).priority} ${ensureFqdn(
        (record.value as MxValue).exchange,
      )}`;
    case "SRV":
      return `${(record.value as SrvValue).priority} ${
        (record.value as SrvValue).weight
      } ${(record.value as SrvValue).port} ${ensureFqdn(
        (record.value as SrvValue).target,
      )}`;
    case "CNAME":
      return ensureFqdn(String(record.value));
    default:
      return String(record.value);
  }
}

function renderForwardBlock(pool: ResolverPool): string {
  const endpoints = pool.endpoints
    .map((endpoint) => `${formatIp(endpoint.host)}:${endpoint.port}`)
    .join(" ");
  return `    forward . ${endpoints} {\n        policy sequential\n    }`;
}

function ownerName(name: string, zone: string): string {
  if (name === zone) return "@";
  return name.endsWith(`.${zone}`) ? name.slice(0, -(zone.length + 1)) : name;
}

function ensureFqdn(value: string): string {
  return value.endsWith(".") ? value : `${value}.`;
}

function formatIp(ip: string): string {
  return ip.includes(":") ? `[${ip}]` : ip;
}
