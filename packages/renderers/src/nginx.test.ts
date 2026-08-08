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
            nginxDirectives:
              'client_max_body_size 100m;\nadd_header X-Robots-Tag "noindex" always;',
            websocket: true,
            pathRules: [
              {
                kind: "exact",
                pattern: "/health",
                action: { type: "redirect", status: 307, location: "/ready" },
              },
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
      candidatePath: "/var/lib/proxycore/candidates/rev-1/nginx",
    });

    expect(candidate.config).toContain("server_name api.home.arpa;");
    expect(candidate.config).toContain("proxy_pass http://10.0.0.20:8080;");
    expect(candidate.config).toContain("location = /health");
    expect(candidate.config).toContain("return 307 /ready;");
    expect(candidate.config).toContain("proxy_set_header x-homelab proxycore;");
    expect(candidate.config).toContain("client_max_body_size 100m;");
    expect(candidate.config).toContain(
      'add_header X-Robots-Tag "noindex" always;',
    );
    expect(candidate.config).not.toContain("client_max_body_size 10m;");
    expect(candidate.config).toContain("listen 0.0.0.0:5432;");
    expect(candidate.config).toContain("return 308 https://$host$request_uri;");
    expect(candidate.config).not.toContain("password");
    expect(candidate.files["nginx.conf"]).toBe(candidate.config);
    expect(candidate.checksum).toHaveLength(64);
  });

  it("renders Basic Auth from candidate files and avoids duplicate root locations", () => {
    const candidate = renderNginxCandidate({
      records: [
        {
          id: "private",
          name: "private.home.arpa",
          type: "A",
          value: "192.168.1.20",
          ttl: 300,
          enabled: true,
          proxied: true,
          proxy: {
            origin: { ip: "10.0.0.40", port: 3000, protocol: "https" },
            tlsEnabled: true,
            certificateId: "cert-private",
            backendTlsVerify: false,
            basicAuth: {
              username: "operator",
              passwordSecretId: "secret-basic-1",
            },
            pathRules: [
              {
                kind: "prefix",
                pattern: "/",
                action: { type: "proxy", rewrite: "/app" },
              },
            ],
          },
        },
      ],
      streams: [],
      capabilities: {
        http3Module: false,
        tcp443Published: true,
        udp443Published: false,
      },
      candidatePath: "/var/lib/proxycore/candidates/rev-2/nginx",
      extraFiles: {
        "basic-auth/secret-basic-1": "operator:{SHA}abc\n",
        "certs/cert-private.crt":
          "-----BEGIN CERTIFICATE-----\nTEST\n-----END CERTIFICATE-----\n",
        "certs/cert-private.key":
          "-----BEGIN PRIVATE KEY-----\nTEST\n-----END PRIVATE KEY-----\n",
      },
    });

    expect(candidate.config).toContain(
      "auth_basic_user_file /var/lib/proxycore/candidates/rev-2/nginx/basic-auth/secret-basic-1;",
    );
    expect(candidate.config).toContain(
      "ssl_certificate /var/lib/proxycore/candidates/rev-2/nginx/certs/cert-private.crt;",
    );
    expect(candidate.config).toContain(
      "proxy_pass https://10.0.0.40:3000/app;",
    );
    expect(candidate.config.match(/location \/\s*\{/g)).toHaveLength(1);
    expect(candidate.files["basic-auth/secret-basic-1"]).toBe(
      "operator:{SHA}abc\n",
    );
    expect(candidate.files["certs/cert-private.crt"]).toContain(
      "BEGIN CERTIFICATE",
    );
    expect(candidate.config).not.toContain("homelab-pass");
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
