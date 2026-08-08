import { beforeEach, describe, expect, it } from "vitest";
import { POST as bootstrap } from "./route";
import { POST as login } from "../login/route";
import { GET as listUsers } from "../../users/route";
import { resetWebContext } from "../../../../server/context";

describe("auth route handlers", () => {
  beforeEach(() => {
    resetWebContext();
  });

  it("bootstraps, logs in, and protects users with the session cookie", async () => {
    const bootstrapResponse = await bootstrap(
      new Request("http://localhost/api/auth/bootstrap", {
        method: "POST",
        body: JSON.stringify({
          username: "owner",
          password: "correct horse battery staple",
        }),
        headers: { "content-type": "application/json" },
      }),
    );
    expect(bootstrapResponse.status).toBe(201);

    const loginResponse = await login(
      new Request("http://localhost/api/auth/login", {
        method: "POST",
        body: JSON.stringify({
          username: "owner",
          password: "correct horse battery staple",
        }),
        headers: { "content-type": "application/json" },
      }),
    );
    expect(loginResponse.status).toBe(200);
    const setCookie = loginResponse.headers.get("set-cookie");
    expect(setCookie).toContain("HttpOnly");

    expect(
      (await listUsers(new Request("http://localhost/api/users"))).status,
    ).toBe(401);
    const usersResponse = await listUsers(
      new Request("http://localhost/api/users", {
        headers: { cookie: setCookie!.split(";")[0] },
      }),
    );
    expect(usersResponse.status).toBe(200);
  });
});
