import { getWebContext } from "../../../../../../server/context";
import { apiError, readJson, requireUser } from "../../../../../../server/http";
import { parseRecordMutationBody } from "../../../../../../server/record-input";

type RouteContext = { params: Promise<{ zoneId: string; recordId: string }> };

export async function PATCH(request: Request, context: RouteContext) {
  try {
    const { user } = await requireUser(request);
    const { zoneId, recordId } = await context.params;
    const body = await readJson(request);
    const record = parseRecordMutationBody({ ...body, id: recordId });
    const zone = await getWebContext().configuration.getZone(zoneId);
    if (!zone.records.some((item) => item.id === recordId)) {
      return Response.json({ error: "Record not found" }, { status: 404 });
    }
    const result = await getWebContext().configuration.addRecord(
      zoneId,
      { ...record, id: recordId },
      user.id,
    );
    return Response.json({ record: result.value, apply: result.apply });
  } catch (error) {
    return apiError(error);
  }
}
