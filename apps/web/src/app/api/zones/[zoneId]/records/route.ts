import { getWebContext } from "../../../../../server/context";
import { apiError, readJson, requireUser } from "../../../../../server/http";
import { parseRecordMutationBody } from "../../../../../server/record-input";

type RouteContext = { params: Promise<{ zoneId: string }> };

export async function GET(request: Request, context: RouteContext) {
  try {
    await requireUser(request);
    const { zoneId } = await context.params;
    return Response.json({
      zone: await getWebContext().configuration.getZone(zoneId),
    });
  } catch (error) {
    return apiError(error);
  }
}

export async function POST(request: Request, context: RouteContext) {
  try {
    const { user } = await requireUser(request);
    const { zoneId } = await context.params;
    const body = await readJson(request);
    const record = parseRecordMutationBody(body);
    const result = await getWebContext().configuration.addRecord(
      zoneId,
      record,
      user.id,
    );
    return Response.json(
      { record: result.value, apply: result.apply },
      { status: 201 },
    );
  } catch (error) {
    return apiError(error);
  }
}
