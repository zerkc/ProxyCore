import { describe, expect, it } from "vitest";
import { renderNginxCandidate } from "./nginx";

describe("Nginx renderer", () => {
  it("renders TLS proxy servers, typed paths, headers, and streams", () => {
    const candidate = renderNginxCandidate({
      records: [
        {
          id: "api",
          name: "api.home.arpa",
          type: "A",
          value: "192.168.1.20",
          ttl: 300,
          enabled: true,
          proxied: true,
          proxy: {
            origin: { ip: "10.0.0.20", port: 8080, protocol: "http" },
            tlsEnabled: true,
            certificateId: "cert-api",
            http2: true,
            headers: [{ name: "X-Homelab", value: "proxycore" }],
            websocket: true,
            pathRules: [
              { kind: "exact", pattern: "/health", action: { type: "redirect", status: 307, location: "/ready" } },
            ],
          },
        },
      ],
      streams: [
        {
          id: "postgres",
          enabled: true,
          protocol: "tcp",
          listenAddress: "0.0.0.0",
          listenPort: 5432,
          upstream: { ip: "10.0.0.30", port: 5432, protocol: "tcp" },
        },
      ],
      capabilities: {
        http3Module: false,
        tcp443Published: true,
        udp443Published: false,
      },
    });

    expect(candidate.config).toContain("server_name api.home.arpa;");
    expect(candidate.config).toContain("proxy_pass http://10.0.0.20:8080;");
    expect(candidate.config).toContain("location = /health");
    expect(candidate.config).toContain("return 307 /ready;");
    expect(candidate.config).toContain("proxy_set_header x-homelab proxycore;");
    expect(candidate.config).toContain("listen 0.0.0.0:5432;");
    expect(candidate.config).not.toContain("password");
    expect(candidate.checksum).toHaveLength(64);
  });

  it("rejects a TLS server without a certificate reference", () => {
    expect(() =>
      renderNginxCandidate({
        records: [
          {
            id: "api",
            name: "api.home.arpa",
            type: "A",
            value: "192.168.1.20",
            ttl: 300,
            enabled: true,
            proxied: true,
            proxy: {
              origin: { ip: "10.0.0.20", port: 8080, protocol: "http" },
              tlsEnabled: true,
            },
          },
        ],
        streams: [],
        capabilities: {
          http3Module: false,
          tcp443Published: true,
          udp443Published: false,
        },
      }),
    ).toThrow(/certificate/i);
  });
});
