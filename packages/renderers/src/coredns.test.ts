import { describe, expect, it } from "vitest";
import { renderCoreDnsCandidate } from "./coredns";

describe("CoreDNS renderer", () => {
  it("renders managed zones, forwarding, and proxied ingress answers deterministically", () => {
    const candidate = renderCoreDnsCandidate({
      zones: [
        {
          name: "home.arpa",
          records: [
            {
              id: "b",
              name: "api.home.arpa",
              type: "A",
              value: "192.168.1.20",
              ttl: 300,
              enabled: true,
              proxied: true,
              proxy: {
                origin: { ip: "192.168.1.20", port: 8080, protocol: "http" },
                tlsEnabled: true,
              },
            },
            {
              id: "a",
              name: "disabled.home.arpa",
              type: "A",
              value: "192.168.1.21",
              ttl: 300,
              enabled: false,
              proxied: false,
            },
          ],
        },
      ],
      ingress: { ipv4: "192.168.1.10" },
      defaultPool: {
        id: "default",
        endpoints: [{ host: "192.168.1.1", port: 53 }],
      },
      forwardingRules: [
        {
          suffix: "corp",
          pool: {
            id: "corp",
            endpoints: [{ host: "10.0.0.53", port: 53 }],
          },
        },
      ],
    });

    const zone = candidate.files["zones/home.arpa.zone"];
    expect(zone).toContain("api 300 IN A 192.168.1.10");
    expect(zone).not.toContain("192.168.1.20");
    expect(zone).not.toContain("disabled");
    expect(candidate.corefile).toContain("file /etc/coredns/zones/home.arpa.zone");
    expect(candidate.corefile).toContain("forward . 10.0.0.53:53");
    expect(candidate.checksum).toHaveLength(64);
  });

  it("renders a proxied CNAME as ingress records instead of an origin CNAME", () => {
    const candidate = renderCoreDnsCandidate({
      zones: [
        {
          name: "home.arpa",
          records: [
            {
              id: "cname",
              name: "service.home.arpa",
              type: "CNAME",
              value: "origin.home.arpa",
              ttl: 300,
              enabled: true,
              proxied: true,
              proxy: {
                origin: { ip: "10.0.0.10", port: 8080, protocol: "http" },
                tlsEnabled: true,
              },
            },
          ],
        },
      ],
      ingress: { ipv4: "192.168.1.10", ipv6: "fd00::10" },
      defaultPool: {
        id: "default",
        endpoints: [{ host: "192.168.1.1", port: 53 }],
      },
      forwardingRules: [],
    });

    const zone = candidate.files["zones/home.arpa.zone"];
    expect(zone).toContain("service 300 IN A 192.168.1.10");
    expect(zone).toContain("service 300 IN AAAA fd00::10");
    expect(zone).not.toContain("origin.home.arpa");
  });
});
