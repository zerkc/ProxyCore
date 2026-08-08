import { getWebContext } from "../../../server/context";
import { apiError, readJson, requireUser } from "../../../server/http";

export async function GET(request: Request) {
  try {
    await requireUser(request);
    return Response.json({
      certificates: getWebContext().configuration.listCertificates(),
    });
  } catch (error) {
    return apiError(error);
  }
}

export async function POST(request: Request) {
  try {
    await requireUser(request);
    const body = await readJson(request);
    if (
      !Array.isArray(body.hostnames) ||
      body.hostnames.some((hostname) => typeof hostname !== "string") ||
      (body.issuer !== "self-signed" && body.issuer !== "letsencrypt") ||
      (body.challenge !== "none" && body.challenge !== "http-01" && body.challenge !== "dns-01")
    ) {
      return Response.json({ error: "Invalid certificate request" }, { status: 400 });
    }
    const certificate = await getWebContext().configuration.issueCertificate({
      hostnames: body.hostnames,
      issuer: body.issuer,
      challenge: body.challenge,
      environment: typeof body.environment === "string" ? body.environment : undefined,
    });
    return Response.json({ certificate }, { status: 202 });
  } catch (error) {
    return apiError(error);
  }
}
