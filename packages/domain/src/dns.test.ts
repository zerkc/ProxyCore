import { describe, expect, it } from "vitest";
import {
  selectForwardingPool,
  validateRecordSet,
  validateResolverPool,
} from "./dns";

describe("DNS domain rules", () => {
  it("normalizes names and applies TTL defaults", () => {
    const [record] = validateRecordSet(
      [
        {
          id: "record-1",
          name: "WWW",
          type: "A",
          value: "192.168.1.20",
          ttl: undefined,
          enabled: true,
          proxied: false,
        },
      ],
      { zoneName: "Home.ARPA" },
    );

    expect(record.name).toBe("www.home.arpa");
    expect(record.ttl).toBe(300);
  });

  it("rejects CNAME coexistence and duplicate proxied hosts", () => {
    expect(() =>
      validateRecordSet(
        [
          {
            id: "cname",
            name: "gateway",
            type: "CNAME",
            value: "router.home.arpa",
            enabled: true,
            proxied: false,
          },
          {
            id: "a",
            name: "gateway",
            type: "A",
            value: "192.168.1.20",
            enabled: true,
            proxied: false,
          },
        ],
        { zoneName: "home.arpa" },
      ),
    ).toThrow(/CNAME/i);

    expect(() =>
      validateRecordSet(
        [
          {
            id: "a1",
            name: "api",
            type: "A",
            value: "192.168.1.20",
            enabled: true,
            proxied: true,
            proxy: {
              origin: { ip: "192.168.1.20", port: 8080, protocol: "http" },
              tlsEnabled: true,
            },
          },
          {
            id: "a2",
            name: "api",
            type: "AAAA",
            value: "fd00::20",
            enabled: true,
            proxied: true,
            proxy: {
              origin: { ip: "fd00::20", port: 8080, protocol: "http" },
              tlsEnabled: true,
            },
          },
        ],
        {
          zoneName: "home.arpa",
          ingress: { ipv4: "192.168.1.10", ipv6: "fd00::10" },
        },
      ),
    ).toThrow(/proxied/i);
  });

  it("selects the most specific forwarding suffix", () => {
    const pool = validateResolverPool({
      id: "default",
      endpoints: [
        { host: "192.168.1.1", port: 53 },
        { host: "192.168.1.2", port: 53 },
      ],
    });
    const corp = validateResolverPool({
      id: "corp",
      endpoints: [{ host: "10.0.0.53", port: 53 }],
    });

    expect(
      selectForwardingPool("printer.eu.corp", [
        { suffix: "corp", pool: corp },
        { suffix: "eu.corp", pool: pool },
      ]),
    ).toEqual(pool);
    expect(selectForwardingPool("example.net", [])).toBeUndefined();
  });
});
