import { getWebContext } from "../../../server/context";
import { apiError, readJson, requireUser } from "../../../server/http";

export async function GET(request: Request) {
  try {
    await requireUser(request);
    return Response.json({ streams: await getWebContext().configuration.listStreams() });
  } catch (error) {
    return apiError(error);
  }
}

export async function POST(request: Request) {
  try {
    await requireUser(request);
    const body = await readJson(request);
    const route = await getWebContext().configuration.addStream({
      id: typeof body.id === "string" ? body.id : undefined,
      enabled: typeof body.enabled === "boolean" ? body.enabled : true,
      protocol: body.protocol === "udp" ? "udp" : "tcp",
      listenAddress: String(body.listenAddress ?? ""),
      listenPort: Number(body.listenPort),
      upstream: body.upstream as {
        ip: string;
        port: number;
        protocol: "tcp" | "udp";
      },
    });
    return Response.json({ stream: route }, { status: 201 });
  } catch (error) {
    return apiError(error);
  }
}
