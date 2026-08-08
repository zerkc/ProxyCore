import { MVP_RECORD_TYPES, type DnsRecordInput } from "@proxycore/domain";
import { getWebContext } from "../../../../../server/context";
import { apiError, readJson, requireUser } from "../../../../../server/http";

type RouteContext = { params: Promise<{ zoneId: string }> };

export async function GET(request: Request, context: RouteContext) {
  try {
    await requireUser(request);
    const { zoneId } = await context.params;
    return Response.json({ zone: getWebContext().configuration.getZone(zoneId) });
  } catch (error) {
    return apiError(error);
  }
}

export async function POST(request: Request, context: RouteContext) {
  try {
    await requireUser(request);
    const { zoneId } = await context.params;
    const body = await readJson(request);
    if (
      typeof body.name !== "string" ||
      typeof body.type !== "string" ||
      !MVP_RECORD_TYPES.includes(body.type as (typeof MVP_RECORD_TYPES)[number])
    ) {
      return Response.json({ error: "name and a supported record type are required" }, { status: 400 });
    }
    const record: Omit<DnsRecordInput, "id"> & { id?: string } = {
      id: typeof body.id === "string" ? body.id : undefined,
      name: body.name,
      type: body.type as DnsRecordInput["type"],
      value: body.value as DnsRecordInput["value"],
      ttl: typeof body.ttl === "number" ? body.ttl : undefined,
      enabled: typeof body.enabled === "boolean" ? body.enabled : true,
      comment: typeof body.comment === "string" ? body.comment : undefined,
      proxied: typeof body.proxied === "boolean" ? body.proxied : false,
      proxy: body.proxy as DnsRecordInput["proxy"],
    };
    return Response.json(
      { record: getWebContext().configuration.addRecord(zoneId, record) },
      { status: 201 },
    );
  } catch (error) {
    return apiError(error);
  }
}
