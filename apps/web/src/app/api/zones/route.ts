import { getWebContext } from "../../../server/context";
import { apiError, readJson, requireUser } from "../../../server/http";

export async function GET(request: Request) {
  try {
    await requireUser(request);
    return Response.json({ zones: getWebContext().configuration.listZones() });
  } catch (error) {
    return apiError(error);
  }
}

export async function POST(request: Request) {
  try {
    await requireUser(request);
    const body = await readJson(request);
    if (typeof body.name !== "string") {
      return Response.json({ error: "Zone name is required" }, { status: 400 });
    }
    return Response.json(
      { zone: getWebContext().configuration.createZone(body.name) },
      { status: 201 },
    );
  } catch (error) {
    return apiError(error);
  }
}
