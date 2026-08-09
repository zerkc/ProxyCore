import { getWebContext } from "../../../../server/context";
import { apiError, HttpError, readJson, requireUser } from "../../../../server/http";

type RouteContext = {
  params: Promise<{ streamId: string }>;
};

function parseStreamBody(body: Record<string, unknown>, streamId: string) {
  return {
    id: streamId,
    enabled: typeof body.enabled === "boolean" ? body.enabled : true,
    protocol: body.protocol === "udp" ? ("udp" as const) : ("tcp" as const),
    listenAddress: String(body.listenAddress ?? ""),
    listenPort: Number(body.listenPort),
    upstream: body.upstream as {
      ip: string;
      port: number;
      protocol: "tcp" | "udp";
    },
  };
}

export async function PATCH(request: Request, context: RouteContext) {
  try {
    await requireUser(request);
    const { streamId } = await context.params;
    const existing = (await getWebContext().configuration.listStreams()).find(
      (stream) => stream.id === streamId,
    );
    if (!existing) {
      throw new HttpError(404, "Stream not found");
    }
    const body = await readJson(request);
    const stream = await getWebContext().configuration.addStream(
      parseStreamBody(
        {
          enabled:
            typeof body.enabled === "boolean" ? body.enabled : existing.enabled,
          protocol: body.protocol ?? existing.protocol,
          listenAddress: body.listenAddress ?? existing.listenAddress,
          listenPort: body.listenPort ?? existing.listenPort,
          upstream: body.upstream ?? existing.upstream,
        },
        streamId,
      ),
    );
    return Response.json({ stream });
  } catch (error) {
    return apiError(error);
  }
}

export async function DELETE(request: Request, context: RouteContext) {
  try {
    await requireUser(request);
    const { streamId } = await context.params;
    const existing = (await getWebContext().configuration.listStreams()).find(
      (stream) => stream.id === streamId,
    );
    if (!existing) {
      throw new HttpError(404, "Stream not found");
    }
    await getWebContext().configuration.deleteStream(streamId);
    return Response.json({ ok: true });
  } catch (error) {
    return apiError(error);
  }
}
