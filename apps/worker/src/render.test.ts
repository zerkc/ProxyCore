import { randomBytes } from "node:crypto";
import { describe, expect, it } from "vitest";
import { InMemorySecretStore } from "@proxycore/certificates";
import { hashBasicAuthPassword } from "@proxycore/crypto";
import { renderJobCandidates } from "./render";

describe("worker candidate rendering", () => {
  it("includes Basic Auth and TLS files beside the Nginx candidate", async () => {
    const masterKey = randomBytes(32).toString("base64");
    const secrets = new InMemorySecretStore(masterKey);
    const secretId = await secrets.put(
      "basic-auth-password",
      hashBasicAuthPassword("homelab-pass"),
    );
    const keySecretId = await secrets.put(
      "certificate-private-key",
      "-----BEGIN PRIVATE KEY-----\nTEST\n-----END PRIVATE KEY-----\n",
    );
    const candidates = await renderJobCandidates(
      {
        settings: {
          ingress: { ipv4: "192.0.2.10" },
          defaultPool: {
            id: "default",
            endpoints: [{ host: "1.1.1.1", port: 53 }],
          },
          forwardingRules: [],
          retentionMaxAgeDays: 7,
          retentionMaxSizeMb: 50,
        },
        zones: [
          {
            id: "zone-1",
            name: "example.test",
            enabled: true,
            records: [
              {
                id: "record-1",
                name: "app.example.test",
                type: "A",
                value: "192.0.2.20",
                ttl: 300,
                enabled: true,
                proxied: true,
                proxy: {
                  origin: { ip: "192.0.2.20", port: 8080, protocol: "http" },
                  tlsEnabled: true,
                  certificateId: "cert-1",
                  basicAuth: {
                    username: "operator",
                    passwordSecretId: secretId,
                  },
                },
              },
            ],
          },
        ],
        streams: [],
        certificates: [
          {
            id: "cert-1",
            hostnames: ["app.example.test"],
            issuer: "self-signed",
            challenge: "none",
            environment: "staging",
            status: "active",
            secretId: keySecretId,
            certificatePem:
              "-----BEGIN CERTIFICATE-----\nTEST\n-----END CERTIFICATE-----\n",
          },
        ],
      },
      {
        id: "job-1",
        revisionId: "revision1",
        target: "nginx",
        status: "queued",
        correlationId: "c-1",
        createdAt: new Date(),
      },
      {
        candidateRoot: "/var/lib/proxycore/candidates",
        secretStore: secrets,
      },
    );

    const nginx = Array.isArray(candidates) ? candidates[0] : candidates;
    expect(nginx.files?.[`basic-auth/${secretId}`]).toMatch(
      /^operator:\{SHA\}/,
    );
    expect(nginx.files?.["certs/cert-1.crt"]).toContain("BEGIN CERTIFICATE");
    expect(nginx.files?.["certs/cert-1.key"]).toContain("BEGIN PRIVATE KEY");
    expect(nginx.files?.["nginx.conf"]).toContain(
      `/var/lib/proxycore/candidates/revision1/nginx/basic-auth/${secretId}`,
    );
    expect(nginx.files?.["nginx.conf"]).toContain(
      "/var/lib/proxycore/candidates/revision1/nginx/certs/cert-1.crt",
    );
    expect(JSON.stringify(nginx.files)).not.toContain("homelab-pass");
  });

  it("fails closed when Basic Auth is configured without a secret store", async () => {
    await expect(
      renderJobCandidates(
        {
          settings: {
            ingress: { ipv4: "192.0.2.10" },
            defaultPool: {
              id: "default",
              endpoints: [{ host: "1.1.1.1", port: 53 }],
            },
            forwardingRules: [],
            retentionMaxAgeDays: 7,
            retentionMaxSizeMb: 50,
          },
          zones: [
            {
              id: "zone-1",
              name: "example.test",
              enabled: true,
              records: [
                {
                  id: "record-1",
                  name: "app.example.test",
                  type: "A",
                  value: "192.0.2.20",
                  ttl: 300,
                  enabled: true,
                  proxied: true,
                  proxy: {
                    origin: { ip: "192.0.2.20", port: 8080, protocol: "http" },
                    tlsEnabled: true,
                    certificateId: "cert-1",
                    basicAuth: {
                      username: "operator",
                      passwordSecretId: "secret-missing",
                    },
                  },
                },
              ],
            },
          ],
          streams: [],
          certificates: [],
        },
        {
          id: "job-1",
          revisionId: "revision1",
          target: "nginx",
          status: "queued",
          correlationId: "c-1",
          createdAt: new Date(),
        },
      ),
    ).rejects.toThrow(/secret store/i);
  });
});
