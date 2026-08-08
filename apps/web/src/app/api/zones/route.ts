import { getWebContext } from "../../../server/context";
import { apiError, readJson, requireUser } from "../../../server/http";

export async function GET(request: Request) {
  try {
    await requireUser(request);
    return Response.json({ zones: await getWebContext().configuration.listZones() });
  } catch (error) {
    return apiError(error);
  }
}

export async function POST(request: Request) {
  try {
    const { user } = await requireUser(request);
    const body = await readJson(request);
    if (typeof body.name !== "string") {
      return Response.json({ error: "Zone name is required" }, { status: 400 });
    }
    const result = await getWebContext().configuration.createZone(body.name, user.id);
    return Response.json({ zone: result.value, apply: result.apply }, { status: 201 });
  } catch (error) {
    return apiError(error);
  }
}
