import { getWebContext } from "../../../../server/context";
import {
  apiError,
  readJson,
  sessionCookie,
} from "../../../../server/http";

export async function POST(request: Request) {
  try {
    const body = await readJson(request);
    const username = typeof body.username === "string" ? body.username : "";
    const password = typeof body.password === "string" ? body.password : "";
    const session = await getWebContext().auth.login(username, password);
    const response = Response.json({ user: session.user, expiresAt: session.expiresAt });
    response.headers.set("Set-Cookie", sessionCookie(session.token, session.expiresAt));
    return response;
  } catch (error) {
    return apiError(error);
  }
}
