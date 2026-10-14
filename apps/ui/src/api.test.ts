import { afterEach, describe, expect, it, vi } from "vitest";
import {
  ApiError,
  BackupApiError,
  EnrollmentApiError,
  confirmEnrollment,
  createEnrollmentToken,
  downloadEnrollmentTrustCA,
  draftEnrollment,
  encodeBackupPassphrase,
  exportBackup,
  getEnrollmentTrust,
  importBackup,
  listEnrollmentTokens,
  previewEnrollment,
  recoverEnrollment,
  revokeEnrollmentToken,
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

describe("enrollment workflow API client", () => {
  const plaintext = "pcenr1_selector_secret-must-not-appear";

  function errorResponse(status: number) {
    return new Response(JSON.stringify({ error: `server detail ${plaintext}` }), {
      status,
      headers: { "content-type": "application/json" },
    });
  }

  async function expectTypedFailure(
    operation: () => Promise<unknown>,
    status: number,
  ) {
    const error = await operation().catch((caught: unknown) => caught);
    expect(error).toBeInstanceOf(EnrollmentApiError);
    expect(error).toMatchObject({ status });
    expect((error as Error).message).not.toContain(plaintext);
  }

  it("creates a token with the session cookie and returns the plaintext once", async () => {
    const body = {
      id: "token-id",
      selector: "selector",
      token: plaintext,
      expiresAt: "2030-01-01T00:00:00Z",
    };
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify(body), { status: 201 }),
    );
    globalThis.fetch = fetchMock as unknown as typeof fetch;

    await expect(createEnrollmentToken()).resolves.toEqual(body);
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/enrollment/tokens",
      expect.objectContaining({
        method: "POST",
        credentials: "include",
        redirect: "error",
        body: "{}",
      }),
    );
    expect(fetchMock.mock.calls[0]![1]).not.toHaveProperty("headers.Authorization");
  });

  it.each([401, 403, 409, 500])("maps token creation status %s", async (status) => {
    globalThis.fetch = vi.fn().mockResolvedValue(errorResponse(status)) as unknown as typeof fetch;
    await expectTypedFailure(() => createEnrollmentToken(), status);
  });

  it("lists token lifecycle summaries without a plaintext field", async () => {
    const body = [
      {
        id: "token-id",
        selector: "selector",
        createdAt: "2030-01-01T00:00:00Z",
        expiresAt: "2030-01-02T00:00:00Z",
        consumedAt: null,
        revokedAt: null,
      },
    ];
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify(body)));
    globalThis.fetch = fetchMock as unknown as typeof fetch;

    await expect(listEnrollmentTokens()).resolves.toEqual(body);
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/enrollment/tokens",
      expect.objectContaining({ method: "GET", credentials: "include", redirect: "error" }),
    );
  });

  it.each([401, 403, 500])("maps token list status %s", async (status) => {
    globalThis.fetch = vi.fn().mockResolvedValue(errorResponse(status)) as unknown as typeof fetch;
    await expectTypedFailure(() => listEnrollmentTokens(), status);
  });

  it("revokes with the exact confirmation query and maps rejection", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(null, { status: 204 }));
    globalThis.fetch = fetchMock as unknown as typeof fetch;

    await expect(revokeEnrollmentToken("token/id", "replace-and-revoke")).resolves.toBeUndefined();
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/enrollment/tokens/token%2Fid/revoke?confirm=replace-and-revoke",
      expect.objectContaining({ method: "POST", credentials: "include", redirect: "error" }),
    );

    globalThis.fetch = vi.fn().mockResolvedValue(errorResponse(409)) as unknown as typeof fetch;
    await expectTypedFailure(
      () => revokeEnrollmentToken("token-id", "replace-and-revoke"),
      409,
    );
    for (const status of [401, 403]) {
      globalThis.fetch = vi.fn().mockResolvedValue(errorResponse(status)) as unknown as typeof fetch;
      await expectTypedFailure(
        () => revokeEnrollmentToken("token-id", "replace-and-revoke"),
        status,
      );
    }
  });

  it("drafts a redacted preview and maps invalid, ineligible, revoked, and server failures", async () => {
    const body = {
      draftId: "draft-id",
      envelopePreview: {
        ingress: { ipv4: "192.0.2.10" },
        role: "node",
        generation: 4,
        clusterKeyId: "cluster-key-id",
        contentHash: "hash",
      },
      expiresAt: "2030-01-01T00:00:00Z",
    };
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify(body)));
    globalThis.fetch = fetchMock as unknown as typeof fetch;
    await expect(draftEnrollment(plaintext, "https://primary.example")).resolves.toEqual(body);
    expect(fetchMock.mock.calls[0]![0]).toBe("/api/topology/enrollment/draft");
    expect(fetchMock.mock.calls[0]![1]).toMatchObject({
      method: "POST",
      credentials: "include",
      redirect: "error",
      body: JSON.stringify({ token: plaintext, primaryURL: "https://primary.example" }),
    });

    for (const status of [400, 403, 410, 500]) {
      globalThis.fetch = vi.fn().mockResolvedValue(errorResponse(status)) as unknown as typeof fetch;
      await expectTypedFailure(
        () => draftEnrollment(plaintext, "https://primary.example"),
        status,
      );
    }
  });

  it("previews an existing draft and maps missing drafts and server failures", async () => {
    const body = { draftId: "draft-id", envelopePreview: {}, expiresAt: "2030-01-01T00:00:00Z" };
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify(body)));
    globalThis.fetch = fetchMock as unknown as typeof fetch;
    await expect(previewEnrollment("draft-id", "node-id", "192.0.2.10")).resolves.toEqual(body);
    expect(fetchMock.mock.calls[0]![1]).toMatchObject({
      body: JSON.stringify({ draftId: "draft-id", requestedNodeID: "node-id", requestedIngress: { ipv4: "192.0.2.10" } }),
    });
    for (const status of [404, 500]) {
      globalThis.fetch = vi.fn().mockResolvedValue(errorResponse(status)) as unknown as typeof fetch;
      await expectTypedFailure(() => previewEnrollment("draft-id"), status);
    }
  });

  it("confirms a draft and maps identity-denied, revoked, and server failures", async () => {
    const body = {
      role: "node",
      generation: 4,
      nodeId: "node-id",
      clusterKeyId: "cluster-key-id",
      archiveId: "archive-id",
      applyJobId: "job-id",
    };
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify(body)));
    globalThis.fetch = fetchMock as unknown as typeof fetch;
    await expect(confirmEnrollment("draft-id")).resolves.toEqual(body);
    expect(fetchMock.mock.calls[0]![1]).toMatchObject({
      body: JSON.stringify({ draftId: "draft-id" }),
    });
    for (const status of [409, 410, 500]) {
      globalThis.fetch = vi.fn().mockResolvedValue(errorResponse(status)) as unknown as typeof fetch;
      await expectTypedFailure(() => confirmEnrollment("draft-id"), status);
    }
  });

  it("recovers a cached draft, exposes the token-missing sentinel, and redacts failures", async () => {
    const body = { draftId: "draft-id", envelopePreview: {}, expiresAt: "2030-01-01T00:00:00Z" };
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify(body)));
    globalThis.fetch = fetchMock as unknown as typeof fetch;
    await expect(recoverEnrollment(plaintext)).resolves.toEqual(body);
    expect(fetchMock.mock.calls[0]![1]).toMatchObject({
      body: JSON.stringify({ token: plaintext }),
    });

    globalThis.fetch = vi.fn().mockResolvedValue(errorResponse(410)) as unknown as typeof fetch;
    await expect(recoverEnrollment()).resolves.toBe("token-missing");

    globalThis.fetch = vi.fn().mockResolvedValue(errorResponse(500)) as unknown as typeof fetch;
    await expectTypedFailure(() => recoverEnrollment(plaintext), 500);
  });

  it("rejects oversized enrollment responses without exposing their contents", async () => {
    const secret = plaintext + "-oversized";
    globalThis.fetch = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ detail: secret, padding: "x".repeat(70_000) })),
    ) as unknown as typeof fetch;
    const error = await listEnrollmentTokens().catch((caught: unknown) => caught);
    expect(error).toBeInstanceOf(EnrollmentApiError);
    expect(error).toMatchObject({ kind: "payload-too-large" });
    expect((error as Error).message).not.toContain(secret);
  });
});

describe("backup API client", () => {
  it("exports with the base64url passphrase header and uses the response filename", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response("zip-bytes", {
        status: 200,
        headers: {
          "content-type": "application/zip",
          "content-disposition":
            'attachment; filename="proxycore-backup-2030-01-01T00-00-00Z.zip"',
        },
      }),
    );
    globalThis.fetch = fetchMock as unknown as typeof fetch;

    const result = await exportBackup("pässphrase/+?");

    expect(result.filename).toBe("proxycore-backup-2030-01-01T00-00-00Z.zip");
    expect(await result.blob.text()).toBe("zip-bytes");
    const [url, init] = fetchMock.mock.calls[0]! as [string, RequestInit];
    expect(url).toBe("/api/backup/export");
    expect(init.method).toBe("POST");
    expect(init.credentials).toBe("include");
    const header = (init.headers as Record<string, string>)[
      "X-Proxycore-Backup-Passphrase"
    ];
    expect(header).toMatch(/^[A-Za-z0-9_-]+$/);
    expect(encodeBackupPassphrase("pässphrase/+?")).toBe(header);
    const padded = header.padEnd(Math.ceil(header.length / 4) * 4, "=");
    const bytes = Uint8Array.from(
      atob(padded.replace(/-/g, "+").replace(/_/g, "/")),
      (char) => char.charCodeAt(0),
    );
    expect(new TextDecoder().decode(bytes)).toBe("pässphrase/+?");
  });

  it("omits the passphrase header and falls back to a default filename", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue(new Response("zip", { status: 200 }));
    globalThis.fetch = fetchMock as unknown as typeof fetch;

    const result = await exportBackup();

    expect(result.filename).toBe("proxycore-backup.zip");
    const init = fetchMock.mock.calls[0]![1] as RequestInit;
    expect(init.headers).not.toHaveProperty("X-Proxycore-Backup-Passphrase");
  });

  it("rejects path-like filenames from Content-Disposition", async () => {
    globalThis.fetch = vi.fn().mockResolvedValue(
      new Response("zip", {
        status: 200,
        headers: { "content-disposition": 'attachment; filename="../../evil.zip"' },
      }),
    ) as unknown as typeof fetch;

    expect((await exportBackup()).filename).toBe("evil.zip");
  });

  it("imports as multipart without a JSON content type and returns the report", async () => {
    const report = {
      dryRun: true,
      bundleSha256: "ab",
      formatVersion: 1,
      exporterVersion: "0.5.2",
      createdAt: "2030-01-01T00:00:00Z",
      installationId: "inst",
      nodeId: "node",
      tables: [{ name: "zones", rowCount: 2, action: "replace" }],
      envWouldChange: false,
      certsToRestore: [],
      appliedPostImport: false,
    };
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify(report), {
        status: 200,
        headers: { "content-type": "application/json" },
      }),
    );
    globalThis.fetch = fetchMock as unknown as typeof fetch;
    const file = new File(["zip"], "bundle.zip", { type: "application/zip" });

    await expect(
      importBackup(file, { passphrase: "secret", dryRun: true }),
    ).resolves.toEqual(report);

    const [url, init] = fetchMock.mock.calls[0]! as [string, RequestInit];
    expect(url).toBe("/api/backup/import?dry_run=true");
    expect(url).not.toContain("secret");
    expect(init.method).toBe("POST");
    expect(init.credentials).toBe("include");
    expect(init.headers).toBeUndefined();
    const form = init.body as FormData;
    expect(form.get("bundle")).toBeInstanceOf(File);
    expect(form.get("passphrase")).toBe(encodeBackupPassphrase("secret"));
  });

  it("sends dry_run=false and omits an empty passphrase", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue(new Response("{}", { status: 200 }));
    globalThis.fetch = fetchMock as unknown as typeof fetch;

    await importBackup(new File(["zip"], "b.zip"), { dryRun: false });

    expect(fetchMock.mock.calls[0]![0]).toBe("/api/backup/import?dry_run=false");
    const form = (fetchMock.mock.calls[0]![1] as RequestInit).body as FormData;
    expect(form.has("passphrase")).toBe(false);
  });

  it.each([
    [409, { code: "master_key_mismatch", error: "master key mismatch" }, "master_key_mismatch"],
    [401, { error: "passphrase mismatch" }, "passphrase_mismatch"],
    [400, { error: "passphrase required" }, "passphrase_required"],
    [400, { error: "invalid backup archive" }, "invalid_bundle"],
    [400, { error: "unsupported backup version" }, "invalid_bundle"],
    [413, { error: "backup bundle is too large" }, "too_large"],
    [503, { error: "backup service is not configured" }, "unavailable"],
    [403, { error: "forbidden" }, "forbidden"],
    [401, { error: "unauthorized" }, "unauthorized"],
    [500, { error: "boom" }, "unknown"],
  ])("maps status %i %j to kind %s", async (status, body, kind) => {
    globalThis.fetch = vi.fn().mockImplementation(
      async () =>
        new Response(JSON.stringify(body), {
          status,
          headers: { "content-type": "application/json" },
        }),
    ) as unknown as typeof fetch;

    for (const call of [
      () => importBackup(new File(["z"], "b.zip"), { dryRun: true }),
      () => exportBackup(),
    ]) {
      const caught = await call().catch((error: unknown) => error);
      expect(caught).toBeInstanceOf(BackupApiError);
      expect(caught).toBeInstanceOf(ApiError);
      expect((caught as BackupApiError).kind).toBe(kind);
      expect((caught as BackupApiError).status).toBe(status);
    }
  });

  it("reports network failures as unknown without leaking details", async () => {
    globalThis.fetch = vi
      .fn()
      .mockRejectedValue(new TypeError("secret detail")) as unknown as typeof fetch;

    const caught = await exportBackup("pw").catch((error: unknown) => error);

    expect(caught).toBeInstanceOf(BackupApiError);
    expect((caught as BackupApiError).kind).toBe("unknown");
    expect((caught as BackupApiError).message).not.toContain("secret detail");
  });
  it("sends the optional CA with a draft and reads only the two fixed trust errors", async () => {
    const plaintext = "pcenr1_selector_secret-must-not-appear";
    const ca = "-----BEGIN CERTIFICATE-----\nQ0E=\n-----END CERTIFICATE-----";
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ draftId: "d" })));
    globalThis.fetch = fetchMock as unknown as typeof fetch;
    await draftEnrollment(plaintext, "https://primary.example", undefined, ca);
    expect(fetchMock.mock.calls[0]![1]).toMatchObject({
      body: JSON.stringify({ token: plaintext, primaryURL: "https://primary.example", caPem: ca }),
    });

    const failWith = (status: number, error: string) => {
      globalThis.fetch = vi.fn().mockResolvedValue(
        new Response(JSON.stringify({ error }), { status }),
      ) as unknown as typeof fetch;
      return draftEnrollment(plaintext, "https://primary.example").catch((caught) => caught);
    };
    expect(await failWith(409, "primary certificate not trusted; load the primary CA")).toMatchObject({ kind: "untrusted-certificate", status: 409 });
    expect(await failWith(400, "invalid CA certificate")).toMatchObject({ kind: "invalid-ca", status: 400 });
    // Anything else, including inherited object keys, stays a generic status error.
    expect(await failWith(409, "constructor")).toMatchObject({ kind: "conflict" });
    expect(await failWith(409, "primary snapshot denied")).toMatchObject({ kind: "conflict" });
    expect(await failWith(400, "primary certificate not trusted; load the primary CA")).toMatchObject({ kind: "bad-request" });
  });
});
