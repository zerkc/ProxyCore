import { describe, expect, it } from "vitest";
import { renderCoreDnsCandidate } from "./coredns";
import type { DnsRecord, IngressAddresses, ProxySettings } from "@proxycore/domain";

// WU 1.5 — node-local CoreDNS rendering.
//
// The PRD requires that a node's CoreDNS instance answer proxied records
// with its OWN ingress address, while DNS-only answers remain identical
// across nodes. This fixture pair (primary A and node B) exercises that
// contract through the existing renderCoreDnsCandidate helper, with no
// changes to the renderer itself: the per-node ingress is passed as a
// parameter at render time.

const minimalProxy: ProxySettings = {
  origin: { ip: "192.0.1.20", port: 8080, protocol: "http" },
  tlsEnabled: false,
};

const dnsOnlyRecord: DnsRecord = {
  id: "rec-dns-only",
  name: "dns-only",
  zoneName: "home.arpa",
  type: "A",
  value: "192.0.1.10",
  ttl: 300,
  enabled: true,
  proxied: false,
};

const proxiedRecord: DnsRecord = {
  id: "rec-proxied",
  name: "app",
  zoneName: "home.arpa",
  type: "A",
  value: "192.0.1.20",
  ttl: 60,
  enabled: true,
  proxied: true,
  proxy: minimalProxy,
};

const proxiedCnameRecord: DnsRecord = {
  id: "rec-proxied-cname",
  name: "alias",
  zoneName: "home.arpa",
  type: "CNAME",
  value: "origin.home.arpa",
  ttl: 60,
  enabled: true,
  proxied: true,
  proxy: minimalProxy,
};

const zone = { name: "home.arpa", records: [dnsOnlyRecord, proxiedRecord, proxiedCnameRecord] };

const primaryIngress: IngressAddresses = {
  ipv4: "192.0.2.10",
  ipv6: undefined,
};

const nodeBIngress: IngressAddresses = {
  ipv4: "192.0.2.20",
  ipv6: undefined,
};

describe("node-local CoreDNS rendering", () => {
  it("renders DNS-only answers identically on primary A and node B", () => {
    const a = renderCoreDnsCandidate({
      zones: [zone],
      ingress: primaryIngress,
      defaultPool: { id: "default", endpoints: [{ host: "1.1.1.1", port: 53 }] },
      forwardingRules: [],
    });
    const b = renderCoreDnsCandidate({
      zones: [zone],
      ingress: nodeBIngress,
      defaultPool: { id: "default", endpoints: [{ host: "1.1.1.1", port: 53 }] },
      forwardingRules: [],
    });
    // DNS-only lines must match across nodes; proxied lines differ because
    // they answer with the applying node's ingress.
    const linesA = a.files["zones/home.arpa.zone"].split("\n");
    const linesB = b.files["zones/home.arpa.zone"].split("\n");
    const dnsOnlyLines = (lines: string[]) =>
      lines.filter((line) => line.includes("dns-only"));
    expect(dnsOnlyLines(linesA)).toEqual(dnsOnlyLines(linesB));
  });

  it("renders proxied answers with the applying node's ingress", () => {
    const a = renderCoreDnsCandidate({
      zones: [zone],
      ingress: primaryIngress,
      defaultPool: { id: "default", endpoints: [{ host: "1.1.1.1", port: 53 }] },
      forwardingRules: [],
    });
    const b = renderCoreDnsCandidate({
      zones: [zone],
      ingress: nodeBIngress,
      defaultPool: { id: "default", endpoints: [{ host: "1.1.1.1", port: 53 }] },
      forwardingRules: [],
    });
    expect(a.files["zones/home.arpa.zone"]).toContain("app 60 IN A 192.0.2.10");
    expect(a.files["zones/home.arpa.zone"]).not.toContain("192.0.2.20");
    expect(b.files["zones/home.arpa.zone"]).toContain("app 60 IN A 192.0.2.20");
    expect(b.files["zones/home.arpa.zone"]).not.toContain("192.0.2.10");
  });

  it("renders proxied CNAMEs with the applying node's ingress", () => {
    const a = renderCoreDnsCandidate({
      zones: [zone],
      ingress: primaryIngress,
      defaultPool: { id: "default", endpoints: [{ host: "1.1.1.1", port: 53 }] },
      forwardingRules: [],
    });
    const b = renderCoreDnsCandidate({
      zones: [zone],
      ingress: nodeBIngress,
      defaultPool: { id: "default", endpoints: [{ host: "1.1.1.1", port: 53 }] },
      forwardingRules: [],
    });
    expect(a.files["zones/home.arpa.zone"]).toContain("alias 60 IN A 192.0.2.10");
    expect(b.files["zones/home.arpa.zone"]).toContain("alias 60 IN A 192.0.2.20");
  });

  it("dual-stack: AAAA proxied records use the node's ipv6 ingress", () => {
    const aaaaZone = {
      name: "home.arpa",
      records: [
        {
          id: "rec-aaaa",
          name: "v6",
          zoneName: "home.arpa",
          type: "AAAA" as const,
          value: "2001:db8::20",
          ttl: 60,
          enabled: true,
          proxied: true,
          proxy: minimalProxy,
        },
      ],
    };
    const nodeA = renderCoreDnsCandidate({
      zones: [aaaaZone],
      ingress: { ipv4: "192.0.2.10", ipv6: "2001:db8::10" },
      defaultPool: { id: "default", endpoints: [{ host: "1.1.1.1", port: 53 }] },
      forwardingRules: [],
    });
    expect(nodeA.files["zones/home.arpa.zone"]).toContain("v6 60 IN AAAA 2001:db8::10");
  });

  it("checksum differs between nodes because the candidate is the whole tree", () => {
    const a = renderCoreDnsCandidate({
      zones: [zone],
      ingress: primaryIngress,
      defaultPool: { id: "default", endpoints: [{ host: "1.1.1.1", port: 53 }] },
      forwardingRules: [],
    });
    const b = renderCoreDnsCandidate({
      zones: [zone],
      ingress: nodeBIngress,
      defaultPool: { id: "default", endpoints: [{ host: "1.1.1.1", port: 53 }] },
      forwardingRules: [],
    });
    // The renderer hashes corefile + every file. Even though DNS-only
    // answers match, the zone file differs in proxied lines, so the
    // checksums differ. This documents that the candidate is per-node.
    expect(a.checksum).not.toBe(b.checksum);
  });

  it("checksum is stable for the same ingress on the same zone set", () => {
    const a = renderCoreDnsCandidate({
      zones: [zone],
      ingress: primaryIngress,
      defaultPool: { id: "default", endpoints: [{ host: "1.1.1.1", port: 53 }] },
      forwardingRules: [],
    });
    const aAgain = renderCoreDnsCandidate({
      zones: [zone],
      ingress: primaryIngress,
      defaultPool: { id: "default", endpoints: [{ host: "1.1.1.1", port: 53 }] },
      forwardingRules: [],
    });
    expect(a.checksum).toBe(aAgain.checksum);
  });
});
