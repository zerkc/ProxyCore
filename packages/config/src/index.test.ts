import { describe, expect, it } from "vitest";
import { loadConfig } from "./index";

describe("loadConfig", () => {
  it("provides safe development defaults", () => {
    const config = loadConfig({ NODE_ENV: "test" });

    expect(config.nodeEnv).toBe("test");
    expect(config.sessionCookieName).toBe("proxycore_session");
    expect(config.sessionTtlSeconds).toBe(28_800);
    expect(config.cloudflare.apiToken).toBeUndefined();
  });

  it("rejects malformed database URLs", () => {
    expect(() => loadConfig({ DATABASE_URL: "not-a-url" })).toThrow();
  });
});
