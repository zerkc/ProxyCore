import { getWebContext } from "../../../../server/context";
import { apiError, clearSessionCookie, sessionToken } from "../../../../server/http";

export async function POST(request: Request) {
  try {
    const token = sessionToken(request);
    if (token) {
      await getWebContext().auth.logout(token);
    }
    const response = Response.json({ ok: true });
    response.headers.set("Set-Cookie", clearSessionCookie());
    return response;
  } catch (error) {
    return apiError(error);
  }
}
