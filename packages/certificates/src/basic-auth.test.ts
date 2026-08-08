import { randomBytes } from "node:crypto";
import { describe, expect, it } from "vitest";
import {
  materializeBasicAuthFiles,
  resolveProxySettingsInput,
} from "./basic-auth";
import { InMemorySecretStore } from "./index";

describe("basic auth secret materialization", () => {
  it("stores only the password hash and materializes an htpasswd file", async () => {
    const masterKey = randomBytes(32).toString("base64");
    const secrets = new InMemorySecretStore(masterKey);
    const resolved = await resolveProxySettingsInput(
      {
        origin: { ip: "10.0.0.20", port: 8080, protocol: "http" },
        tlsEnabled: true,
        basicAuth: { username: "operator", password: "homelab-pass" },
      },
      { secretStore: secrets },
    );

    expect(resolved?.basicAuth?.passwordSecretId).toBeTruthy();
    expect(JSON.stringify(resolved)).not.toContain("homelab-pass");

    const files = await materializeBasicAuthFiles(
      [{ proxy: resolved }],
      secrets,
    );
    const secretId = resolved!.basicAuth!.passwordSecretId;
    expect(files[`basic-auth/${secretId}`]).toMatch(/^operator:\{SHA\}/);
    expect(files[`basic-auth/${secretId}`]).not.toContain("homelab-pass");
  });

  it("keeps an existing secret when the password is omitted", async () => {
    const masterKey = randomBytes(32).toString("base64");
    const secrets = new InMemorySecretStore(masterKey);
    const created = await resolveProxySettingsInput(
      {
        origin: { ip: "10.0.0.20", port: 8080, protocol: "http" },
        tlsEnabled: true,
        basicAuth: { username: "operator", password: "homelab-pass" },
      },
      { secretStore: secrets },
    );
    const kept = await resolveProxySettingsInput(
      {
        origin: { ip: "10.0.0.20", port: 9090, protocol: "http" },
        tlsEnabled: true,
        basicAuth: { username: "operator" },
      },
      { secretStore: secrets, existing: created },
    );

    expect(kept?.basicAuth?.passwordSecretId).toBe(
      created?.basicAuth?.passwordSecretId,
    );
    expect(kept?.origin.port).toBe(9090);
  });
});
