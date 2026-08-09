import { randomBytes, X509Certificate } from "node:crypto";
import { describe, expect, it } from "vitest";
import {
  CloudflareDns01Adapter,
  FakeDns01Adapter,
  InMemorySecretStore,
  certificateCoversHostnames,
  issueSelfSigned,
  validateUploadedCertificate,
} from "./index";

describe("certificate services", () => {
  it("issues a self-signed certificate and encrypts the private key", async () => {
    const masterKey = randomBytes(32).toString("base64");
    const secrets = new InMemorySecretStore(masterKey);
    const result = await issueSelfSigned(["proxy.home.arpa"], {
      secretStore: secrets,
      masterKeyBase64: masterKey,
    });

    expect(result.certificatePem).toContain("BEGIN CERTIFICATE");
    expect(result.privateKeyPem).toMatch(/BEGIN .*PRIVATE KEY/);
    expect(new X509Certificate(result.certificatePem).subject).toContain(
      "proxy.home.arpa",
    );
    expect(result.secretId).toBeTruthy();
    expect(secrets.raw(result.secretId)).not.toContain("BEGIN PRIVATE KEY");
  });

  it("keeps DNS-01 fake records scoped and supports cleanup", async () => {
    const adapter = new FakeDns01Adapter("example.com");
    await adapter.present("_acme-challenge.api.example.com", "proof");

    await expect(
      adapter.observe("_acme-challenge.api.example.com"),
    ).resolves.toEqual(["proof"]);
    await expect(adapter.present("api.example.net", "bad")).rejects.toThrow(
      /challenge/i,
    );
    await adapter.cleanup("_acme-challenge.api.example.com", "proof");
    await expect(
      adapter.observe("_acme-challenge.api.example.com"),
    ).resolves.toEqual([]);
  });

  it("discovers the Cloudflare zone from the certificate hostname", async () => {
    const requests: string[] = [];
    const fetchImpl: typeof fetch = async (input, init) => {
      const url = String(input);
      requests.push(`${init?.method ?? "GET"} ${url}`);
      if (url.includes("/zones?name=example.com")) {
        return new Response(
          JSON.stringify({
            success: true,
            result: [{ id: "zone-1", name: "example.com" }],
          }),
          { status: 200 },
        );
      }
      if (url.includes("/zones?")) {
        return new Response(JSON.stringify({ success: true, result: [] }), {
          status: 200,
        });
      }
      if (url.endsWith("/dns_records") && init?.method === "POST") {
        return new Response(JSON.stringify({ success: true, result: {} }), {
          status: 200,
        });
      }
      if (url.includes("/dns_records?") && init?.method === "GET") {
        return new Response(
          JSON.stringify({
            success: true,
            result: [{ id: "record-1", type: "TXT", content: "proof" }],
          }),
          { status: 200 },
        );
      }
      return new Response(JSON.stringify({ success: true, result: {} }), {
        status: 200,
      });
    };
    const adapter = new CloudflareDns01Adapter({
      apiToken: "token",
      zoneId: "",
      fetchImpl,
    });

    await adapter.present("api.example.com", "proof");
    await adapter.cleanup("api.example.com", "proof");

    expect(
      requests.some((request) =>
        request.includes(
          "GET https://api.cloudflare.com/client/v4/zones?name=example.com",
        ),
      ),
    ).toBe(true);
    expect(requests).toContain(
      "POST https://api.cloudflare.com/client/v4/zones/zone-1/dns_records",
    );
    expect(requests).toContain(
      "DELETE https://api.cloudflare.com/client/v4/zones/zone-1/dns_records/record-1",
    );
  });

  it("preserves Cloudflare error codes and messages", async () => {
    const fetchImpl: typeof fetch = async () =>
      new Response(
        JSON.stringify({
          success: false,
          result: null,
          errors: [
            {
              code: 81053,
              message: "An A, AAAA, or CNAME record already exists",
            },
          ],
        }),
        { status: 400 },
      );
    const adapter = new CloudflareDns01Adapter({
      apiToken: "token",
      zoneId: "zone-1",
      zoneName: "example.com",
      fetchImpl,
    });

    await expect(adapter.present("api.example.com", "proof")).rejects.toThrow(
      "Cloudflare DNS-01 request failed (400): [81053] An A, AAAA, or CNAME record already exists",
    );
  });

  it("matches exact hostnames and one-label wildcard certificates", () => {
    expect(
      certificateCoversHostnames(["api.example.com"], ["api.example.com"]),
    ).toBe(true);
    expect(
      certificateCoversHostnames(["*.example.com"], ["api.example.com"]),
    ).toBe(true);
    expect(
      certificateCoversHostnames(["*.example.com"], ["deep.api.example.com"]),
    ).toBe(false);
    expect(certificateCoversHostnames(["*.example.com"], ["example.com"])).toBe(
      false,
    );
  });

  it("validates uploaded certificate chains and matching private keys", async () => {
    const masterKey = randomBytes(32).toString("base64");
    const secrets = new InMemorySecretStore(masterKey);
    const generated = await issueSelfSigned(
      ["upload.example.com", "*.upload.example.com"],
      { secretStore: secrets, masterKeyBase64: masterKey },
    );
    const privateKey = await secrets.get(generated.secretId);

    const validated = validateUploadedCertificate(
      ["upload.example.com", "*.upload.example.com"],
      generated.certificatePem,
      privateKey!,
    );

    expect(
      Math.abs(validated.expiresAt.getTime() - generated.expiresAt.getTime()),
    ).toBeLessThan(1_000);
    expect(() =>
      validateUploadedCertificate(
        ["other.example.com"],
        generated.certificatePem,
        privateKey!,
      ),
    ).toThrow(/SANs/i);
  });
});
