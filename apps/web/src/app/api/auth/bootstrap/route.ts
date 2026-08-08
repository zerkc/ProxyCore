import { getWebContext } from "../../../../server/context";
import { apiError, readJson } from "../../../../server/http";

export async function POST(request: Request) {
  try {
    const body = await readJson(request);
    const username = typeof body.username === "string" ? body.username : "";
    const password = typeof body.password === "string" ? body.password : "";
    const user = await getWebContext().auth.bootstrap(username, password);
    return Response.json({ user }, { status: 201 });
  } catch (error) {
    return apiError(error);
  }
}
