import { describe, expect, it } from "vitest";
import { InMemoryAuthStore } from "@proxycore/testing";
import { AuthService } from "./auth";

const password = "correct horse battery staple";

function createService() {
  return new AuthService(new InMemoryAuthStore(), {
    sessionTtlSeconds: 3_600,
  });
}

describe("local authentication", () => {
  it("bootstraps one Owner and rejects a second bootstrap", async () => {
    const service = createService();
    const owner = await service.bootstrap("owner", password);

    expect(owner.role).toBe("owner");
    await expect(service.bootstrap("other", password)).rejects.toThrow(/bootstrap/i);
  });

  it("issues revocable sessions and rejects them after logout", async () => {
    const service = createService();
    await service.bootstrap("owner", password);
    const session = await service.login("owner", password);

    await expect(service.authenticate(session.token)).resolves.toMatchObject({
      username: "owner",
    });
    await service.logout(session.token);
    await expect(service.authenticate(session.token)).rejects.toThrow(/session/i);
  });

  it("allows only Owners to manage users and protects the last Owner", async () => {
    const service = createService();
    await service.bootstrap("owner", password);
    const ownerSession = await service.login("owner", password);
    const operator = await service.createUser(ownerSession.token, {
      username: "operator",
      password,
      role: "operator",
    });
    const operatorSession = await service.login("operator", password);

    await expect(
      service.createUser(operatorSession.token, {
        username: "blocked",
        password,
        role: "operator",
      }),
    ).rejects.toThrow(/permission/i);
    await expect(service.deleteUser(ownerSession.token, operator.id)).resolves.toBeUndefined();
    await expect(service.deleteUser(ownerSession.token, ownerSession.userId)).rejects.toThrow(/last owner/i);
  });
});
