export const dynamic = "force-dynamic";

export function GET() {
  return Response.json({
    status: "ok",
    service: "web",
    timestamp: new Date().toISOString(),
  });
}
