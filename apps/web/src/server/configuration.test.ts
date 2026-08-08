import { randomBytes } from "node:crypto";
import { describe, expect, it } from "vitest";
import { InMemoryConfigurationStore } from "./configuration";

describe("automatic DNS apply", () => {
  it("keeps the detected ingress as the default while allowing an explicit override", async () => {
    const store = new InMemoryConfigurationStore(undefined, {
      ipv4: "192.168.1.10",
    });

    expect((await store.getSettings()).ingress).toEqual({
      ipv4: "192.168.1.10",
    });
    await store.initializeIngress({ ipv4: "192.168.1.20" });
    expect((await store.getSettings()).ingress).toEqual({
      ipv4: "192.168.1.10",
    });

    await store.updateSettings({ ingress: { ipv4: "10.0.0.10" } });
    expect((await store.getSettings()).ingress).toEqual({ ipv4: "10.0.0.10" });
  });

  it("queues an apply when a zone or record is saved", async () => {
    const store = new InMemoryConfigurationStore();
    await store.updateSettings({
      defaultPool: {
        id: "default",
        endpoints: [{ host: "1.1.1.1", port: 53 }],
      },
    });

    const zoneResult = await store.createZone("example.test", "owner-1");
    const recordResult = await store.addRecord(
      zoneResult.value.id,
      {
        name: "app.example.test",
        type: "A",
        value: "192.0.2.10",
        enabled: true,
        proxied: false,
      },
      "owner-1",
    );

    expect(zoneResult.apply.job.status).toBe("queued");
    expect(recordResult.apply.job.status).toBe("queued");
    expect(recordResult.value.name).toBe("app.example.test");
    expect((await store.status()).jobs).toHaveLength(2);
  });

  it("creates and edits proxied records with upstream port and Basic Auth secrets", async () => {
    const masterKey = randomBytes(32).toString("base64");
    const store = new InMemoryConfigurationStore(masterKey, {
      ipv4: "192.0.2.1",
    });
    await store.updateSettings({
      defaultPool: {
        id: "default",
        endpoints: [{ host: "1.1.1.1", port: 53 }],
      },
    });
    const certificate = await store.issueCertificate({
      hostnames: ["app.example.test"],
      issuer: "self-signed",
      challenge: "none",
    });
    const zone = await store.createZone("example.test", "owner-1");

    const created = await store.addRecord(
      zone.value.id,
      {
        name: "app",
        type: "A",
        value: "192.0.2.10",
        enabled: true,
        proxied: true,
        proxy: {
          origin: { ip: "192.0.2.10", port: 8080, protocol: "http" },
          tlsEnabled: true,
          certificateId: certificate.id,
          redirectHttpToHttps: true,
          pathRules: [
            {
              kind: "exact",
              pattern: "/old",
              action: { type: "redirect", status: 301, location: "/new" },
            },
          ],
          basicAuth: {
            username: "operator",
            password: "homelab-secret",
          },
        },
      },
      "owner-1",
    );

    expect(created.value.proxy?.origin.port).toBe(8080);
    expect(created.value.proxy?.basicAuth?.username).toBe("operator");
    expect(created.value.proxy?.basicAuth?.passwordSecretId).toBeTruthy();
    expect(JSON.stringify(created.value)).not.toContain("homelab-secret");

    const secretId = created.value.proxy!.basicAuth!.passwordSecretId;
    const updated = await store.addRecord(
      zone.value.id,
      {
        id: created.value.id,
        name: "app",
        type: "A",
        value: "192.0.2.10",
        enabled: true,
        proxied: true,
        proxy: {
          origin: { ip: "192.0.2.10", port: 9090, protocol: "http" },
          tlsEnabled: true,
          certificateId: certificate.id,
          basicAuth: {
            username: "operator",
            passwordSecretId: secretId,
          },
        },
      },
      "owner-1",
    );

    expect(updated.value.proxy?.origin.port).toBe(9090);
    expect(updated.value.proxy?.basicAuth?.passwordSecretId).toBe(secretId);
    expect((await store.status()).jobs.length).toBeGreaterThanOrEqual(3);
  });
});
