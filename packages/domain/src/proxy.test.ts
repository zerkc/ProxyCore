import { describe, expect, it } from "vitest";
import {
  matchPathRule,
  validatePathRules,
  validateProxySettings,
  validateStreamRoutes,
} from "./proxy";

describe("proxy domain rules", () => {
  it("uses exact paths before the longest prefix", () => {
    const rules = validatePathRules([
      { kind: "prefix", pattern: "/", action: { type: "proxy" } },
      { kind: "prefix", pattern: "/api", action: { type: "proxy" } },
      {
        kind: "exact",
        pattern: "/api/login",
        action: { type: "redirect", status: 307 },
      },
    ]);

    expect(matchPathRule("/api/login", rules)?.kind).toBe("exact");
    expect(matchPathRule("/api/users", rules)?.pattern).toBe("/api");
    expect(matchPathRule("/assets/app.js", rules)?.pattern).toBe("/");
  });

  it("rejects Basic Auth without TLS and HTTP/3 without capabilities", () => {
    expect(() =>
      validateProxySettings({
        origin: { ip: "127.0.0.1", port: 8080, protocol: "http" },
        tlsEnabled: false,
        basicAuth: { username: "admin", passwordSecretId: "secret-1" },
      }),
    ).toThrow(/TLS/i);

    expect(() =>
      validateProxySettings({
        origin: { ip: "127.0.0.1", port: 8080, protocol: "http" },
        tlsEnabled: true,
        basicAuth: { username: "admin", passwordSecretId: "../escape" },
      }),
    ).toThrow(/secret/i);

    expect(() =>
      validateProxySettings(
        {
          origin: { ip: "127.0.0.1", port: 8080, protocol: "http" },
          tlsEnabled: true,
          http3: true,
        },
        { http3Module: false, tcp443Published: true, udp443Published: true },
      ),
    ).toThrow(/HTTP\/3/i);
  });

  it("accepts an explicit upstream port and protocol", () => {
    const settings = validateProxySettings({
      origin: { ip: "10.0.0.20", port: 8443, protocol: "https" },
      tlsEnabled: false,
      backendTlsVerify: false,
    });
    expect(settings.origin.port).toBe(8443);
    expect(settings.origin.protocol).toBe("https");
  });

  it("accepts server-level Nginx directives and rejects configuration blocks", () => {
    const settings = validateProxySettings({
      origin: { ip: "10.0.0.20", port: 8080, protocol: "http" },
      tlsEnabled: false,
      nginxDirectives:
        '\r\nclient_max_body_size 100m;\r\nadd_header X-JSON "{}";\r\n',
    });
    expect(settings.nginxDirectives).toBe(
      'client_max_body_size 100m;\nadd_header X-JSON "{}";',
    );

    expect(() =>
      validateProxySettings({
        origin: { ip: "10.0.0.20", port: 8080, protocol: "http" },
        tlsEnabled: false,
        nginxDirectives: "location / { proxy_pass http://127.0.0.1; }",
      }),
    ).toThrow(/configuration blocks/i);
  });

  it("rejects stream listener conflicts", () => {
    expect(() =>
      validateStreamRoutes([
        {
          id: "stream-1",
          enabled: true,
          protocol: "tcp",
          listenAddress: "0.0.0.0",
          listenPort: 5432,
          upstream: { ip: "10.0.0.10", port: 5432, protocol: "tcp" },
        },
        {
          id: "stream-2",
          enabled: true,
          protocol: "tcp",
          listenAddress: "0.0.0.0",
          listenPort: 5432,
          upstream: { ip: "10.0.0.11", port: 5432, protocol: "tcp" },
        },
      ]),
    ).toThrow(/conflict/i);
  });
});
