import { randomBytes } from "node:crypto";
import { describe, expect, it } from "vitest";
import {
  decryptSecret,
  encryptSecret,
  hashPassword,
  verifyPassword,
} from "./index";

describe("ProxyCore crypto primitives", () => {
  it("hashes and verifies passwords without storing plaintext", async () => {
    const password = "correct horse battery staple";
    const encoded = await hashPassword(password);

    expect(encoded).not.toContain(password);
    await expect(verifyPassword(password, encoded)).resolves.toBe(true);
    await expect(verifyPassword("wrong password", encoded)).resolves.toBe(false);
  });

  it("encrypts and decrypts secret values with an external master key", () => {
    const key = randomBytes(32).toString("base64");
    const encrypted = encryptSecret("cloudflare-token", key);

    expect(encrypted).not.toContain("cloudflare-token");
    expect(decryptSecret(encrypted, key)).toBe("cloudflare-token");
  });
});
