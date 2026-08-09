import { http01ChallengeStore } from "../../../../server/acme";

type RouteContext = { params: Promise<{ token: string }> };

export async function GET(_request: Request, context: RouteContext) {
  const { token } = await context.params;
  const keyAuthorization = http01ChallengeStore.get(token);
  if (!keyAuthorization) {
    return new Response("Not found", {
      status: 404,
      headers: { "cache-control": "no-store" },
    });
  }
  return new Response(keyAuthorization, {
    headers: {
      "cache-control": "no-store",
      "content-type": "text/plain; charset=utf-8",
    },
  });
}
