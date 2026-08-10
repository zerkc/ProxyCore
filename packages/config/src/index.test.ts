import { describe, expect, it } from "vitest";
import { loadConfig } from "./index";

describe("loadConfig", () => {
  it("provides safe development defaults", () => {
    const config = loadConfig({ NODE_ENV: "test" });

    expect(config.nodeEnv).toBe("test");
    expect(config.sessionCookieName).toBe("proxycore_session");
    expect(config.sessionTtlSeconds).toBe(28_800);
    expect(config.secureCookies).toBe(false);
    expect(config.acmeDirectoryUrl).toContain("acme-staging");
  });

  it("enables secure cookies only when explicitly opted in", () => {
    expect(loadConfig({ NODE_ENV: "production" }).secureCookies).toBe(false);
    expect(
      loadConfig({ NODE_ENV: "production", PROXYCORE_SECURE_COOKIES: "1" })
        .secureCookies,
    ).toBe(true);
  });

  it("rejects malformed database URLs", () => {
    expect(() => loadConfig({ DATABASE_URL: "not-a-url" })).toThrow();
  });
});
