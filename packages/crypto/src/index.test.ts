import { randomBytes } from "node:crypto";
import { describe, expect, it } from "vitest";
import {
  decryptSecret,
  encryptSecret,
  formatBasicAuthFileLine,
  hashBasicAuthPassword,
  hashPassword,
  verifyPassword,
} from "./index";

describe("ProxyCore crypto primitives", () => {
  it("hashes and verifies passwords without storing plaintext", async () => {
    const password = "correct horse battery staple";
    const encoded = await hashPassword(password);

    expect(encoded).not.toContain(password);
    await expect(verifyPassword(password, encoded)).resolves.toBe(true);
    await expect(verifyPassword("wrong password", encoded)).resolves.toBe(
      false,
    );
  });

  it("encrypts and decrypts secret values with an external master key", () => {
    const key = randomBytes(32).toString("base64");
    const encrypted = encryptSecret("cloudflare-token", key);

    expect(encrypted).not.toContain("cloudflare-token");
    expect(decryptSecret(encrypted, key)).toBe("cloudflare-token");
  });

  it("hashes Basic Auth passwords into Nginx htpasswd lines without plaintext", () => {
    const password = "homelab-pass";
    const hash = hashBasicAuthPassword(password);
    const line = formatBasicAuthFileLine("operator", hash);

    expect(hash.startsWith("{SHA}")).toBe(true);
    expect(hash).not.toContain(password);
    expect(line).toBe(`operator:${hash}\n`);
    expect(line).not.toContain(password);
  });
});
