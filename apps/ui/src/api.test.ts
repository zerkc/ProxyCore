import { afterEach, describe, expect, it, vi } from "vitest";
import {
  ApiError,
  downloadEnrollmentTrustCA,
  getEnrollmentTrust,
} from "./api";

const originalFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = originalFetch;
});

describe("enrollment trust API client", () => {
  it("loads typed trust metadata with credentials and no-store caching", async () => {
    const body = {
      status: "ready",
      configured: true,
      ready: true,
      caDerSha256: "ab".repeat(32),
      expiresAt: "2030-01-01T00:00:00Z",
      certificatePem: "-----BEGIN CERTIFICATE-----\nleaf\n-----END CERTIFICATE-----",
      caCertificatePem: "-----BEGIN CERTIFICATE-----\nca\n-----END CERTIFICATE-----",
    };
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify(body), {
        status: 200,
        headers: { "content-type": "application/json" },
      }),
    );
    globalThis.fetch = fetchMock as unknown as typeof fetch;

    await expect(getEnrollmentTrust()).resolves.toEqual(body);
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/settings/enrollment-trust",
      expect.objectContaining({
        credentials: "include",
        cache: "no-store",
        headers: { "content-type": "application/json" },
      }),
    );
  });

  it("turns metadata API failures into ApiError with the server message", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ error: "Permission denied" }), {
        status: 403,
        headers: { "content-type": "application/json" },
      }),
    );
    globalThis.fetch = fetchMock as unknown as typeof fetch;

    const error = await getEnrollmentTrust().catch((caught: unknown) => caught);
    expect(error).toBeInstanceOf(ApiError);
    expect(error).toMatchObject({ status: 403, message: "Permission denied" });
  });

  it("downloads binary CA content with no JSON content-type and uses the fixed safe filename", async () => {
    const pem = "-----BEGIN CERTIFICATE-----\nca\n-----END CERTIFICATE-----\n";
    const blob = new Blob([pem], { type: "application/x-pem-file" });
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(blob, {
        status: 200,
        headers: {
          "content-type": "application/x-pem-file",
          "content-disposition": 'attachment; filename="enrollment-ca.pem"',
        },
      }),
    );
    globalThis.fetch = fetchMock as unknown as typeof fetch;

    const result = await downloadEnrollmentTrustCA();
    expect(await result.blob.text()).toBe(pem);
    expect(result.filename).toBe("proxycore-enrollment-ca.pem");
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/settings/enrollment-trust/ca",
      expect.objectContaining({ credentials: "include", cache: "no-store" }),
    );
    expect(fetchMock.mock.calls[0]![1]).not.toHaveProperty("headers");
  });

  it("uses the fixed safe filename for traversal, percent, control, and dot dispositions", async () => {
    for (const disposition of [
      'attachment; filename="../../evil.pem"',
      'attachment; filename="..\\\\evil.pem"',
      "attachment; filename*=UTF-8''%2e%2e%2fevil.pem",
      "attachment; filename*=UTF-8''%00evil.pem",
      'attachment; filename="."',
      'attachment; filename=".."',
    ]) {
      const fetchMock = vi.fn().mockResolvedValue(
        new Response(new Blob(["ca"]), {
          status: 200,
          headers: {
            "content-type": "application/x-pem-file",
            "content-disposition": disposition,
          },
        }),
      );
      globalThis.fetch = fetchMock as unknown as typeof fetch;

      await expect(downloadEnrollmentTrustCA()).resolves.toMatchObject({
        filename: "proxycore-enrollment-ca.pem",
      });
    }
  });

  it("turns binary endpoint failures into ApiError without trying to parse success as JSON", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ error: "enrollment trust is not ready" }), {
        status: 409,
        headers: { "content-type": "application/json" },
      }),
    );
    globalThis.fetch = fetchMock as unknown as typeof fetch;

    await expect(downloadEnrollmentTrustCA()).rejects.toMatchObject({
      status: 409,
      message: "enrollment trust is not ready",
    });
  });
});
