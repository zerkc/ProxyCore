import { getWebContext } from "../../../server/context";
import { apiError, readJson, requireUser } from "../../../server/http";

export async function GET(request: Request) {
  try {
    return Response.json({
      users: await getWebContext().auth.listUsers((await requireUser(request, ["owner"])).token),
    });
  } catch (error) {
    return apiError(error);
  }
}

export async function POST(request: Request) {
  try {
    const { token } = await requireUser(request, ["owner"]);
    const body = await readJson(request);
    if (
      typeof body.username !== "string" ||
      typeof body.password !== "string" ||
      (body.role !== "owner" && body.role !== "operator")
    ) {
      return Response.json({ error: "username, password, and role are required" }, { status: 400 });
    }
    const user = await getWebContext().auth.createUser(token, {
      username: body.username,
      password: body.password,
      role: body.role,
    });
    return Response.json({ user }, { status: 201 });
  } catch (error) {
    return apiError(error);
  }
}
