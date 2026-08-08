import { getWebContext } from "../../../server/context";
import { apiError, readJson, requireUser } from "../../../server/http";

export async function GET(request: Request) {
  try {
    await requireUser(request);
    return Response.json({ settings: await getWebContext().configuration.getSettings() });
  } catch (error) {
    return apiError(error);
  }
}

export async function PUT(request: Request) {
  try {
    await requireUser(request);
    const body = await readJson(request);
    const settings = await getWebContext().configuration.updateSettings({
      ingress:
        body.ingress && typeof body.ingress === "object"
          ? {
              ipv4: readString(body.ingress, "ipv4"),
              ipv6: readString(body.ingress, "ipv6"),
            }
          : undefined,
      defaultPool:
        body.defaultPool && typeof body.defaultPool === "object"
          ? {
              id: readString(body.defaultPool, "id") ?? "default",
              endpoints: readArray(body.defaultPool, "endpoints") as { host: string; port: number }[],
            }
          : undefined,
      forwardingRules: Array.isArray(body.forwardingRules)
        ? (body.forwardingRules as never[])
        : undefined,
      retentionMaxAgeDays:
        typeof body.retentionMaxAgeDays === "number" ? body.retentionMaxAgeDays : undefined,
      retentionMaxSizeMb:
        typeof body.retentionMaxSizeMb === "number" ? body.retentionMaxSizeMb : undefined,
    });
    return Response.json({ settings });
  } catch (error) {
    return apiError(error);
  }
}

function readString(value: object, key: string): string | undefined {
  const entry = (value as Record<string, unknown>)[key];
  return typeof entry === "string" ? entry : undefined;
}

function readArray(value: object, key: string): unknown[] {
  const entry = (value as Record<string, unknown>)[key];
  return Array.isArray(entry) ? entry : [];
}
