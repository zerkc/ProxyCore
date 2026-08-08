import { randomBytes, X509Certificate } from "node:crypto";
import { describe, expect, it } from "vitest";
import {
  FakeDns01Adapter,
  InMemorySecretStore,
  certificateCoversHostnames,
  issueSelfSigned,
} from "./index";

describe("certificate services", () => {
  it("issues a self-signed certificate and encrypts the private key", async () => {
    const masterKey = randomBytes(32).toString("base64");
    const secrets = new InMemorySecretStore(masterKey);
    const result = await issueSelfSigned(
      ["proxy.home.arpa"],
      { secretStore: secrets, masterKeyBase64: masterKey },
    );

    expect(result.certificatePem).toContain("BEGIN CERTIFICATE");
    expect(result.privateKeyPem).toMatch(/BEGIN .*PRIVATE KEY/);
    expect(new X509Certificate(result.certificatePem).subject).toContain("proxy.home.arpa");
    expect(result.secretId).toBeTruthy();
    expect(secrets.raw(result.secretId)).not.toContain("BEGIN PRIVATE KEY");
  });

  it("keeps DNS-01 fake records scoped and supports cleanup", async () => {
    const adapter = new FakeDns01Adapter("example.com");
    await adapter.present("_acme-challenge.api.example.com", "proof");

    await expect(adapter.observe("_acme-challenge.api.example.com")).resolves.toEqual(["proof"]);
    await expect(adapter.present("api.example.net", "bad")).rejects.toThrow(/challenge/i);
    await adapter.cleanup("_acme-challenge.api.example.com", "proof");
    await expect(adapter.observe("_acme-challenge.api.example.com")).resolves.toEqual([]);
  });

  it("matches exact hostnames and one-label wildcard certificates", () => {
    expect(certificateCoversHostnames(["api.example.com"], ["api.example.com"])).toBe(true);
    expect(certificateCoversHostnames(["*.example.com"], ["api.example.com"])).toBe(true);
    expect(certificateCoversHostnames(["*.example.com"], ["deep.api.example.com"])).toBe(false);
    expect(certificateCoversHostnames(["*.example.com"], ["example.com"])).toBe(false);
  });
});
