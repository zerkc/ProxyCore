import { getWebContext } from "../../../../server/context";
import { apiError, readJson, requireUser } from "../../../../server/http";

type RouteContext = { params: Promise<{ userId: string }> };

export async function PATCH(request: Request, context: RouteContext) {
  try {
    const { token } = await requireUser(request, ["owner"]);
    const { userId } = await context.params;
    const body = await readJson(request);
    const user = await getWebContext().auth.updateUser(token, userId, {
      role: body.role === "owner" || body.role === "operator" ? body.role : undefined,
      active: typeof body.active === "boolean" ? body.active : undefined,
      password: typeof body.password === "string" ? body.password : undefined,
    });
    return Response.json({ user });
  } catch (error) {
    return apiError(error);
  }
}

export async function DELETE(request: Request, context: RouteContext) {
  try {
    const { token } = await requireUser(request, ["owner"]);
    const { userId } = await context.params;
    await getWebContext().auth.deleteUser(token, userId);
    return new Response(null, { status: 204 });
  } catch (error) {
    return apiError(error);
  }
}
