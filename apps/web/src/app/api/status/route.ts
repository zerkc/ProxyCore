import { getWebContext } from "../../../server/context";
import { apiError, requireUser } from "../../../server/http";

export async function GET(request: Request) {
  try {
    await requireUser(request);
    const status = await getWebContext().configuration.status();
    return Response.json({
      ...status,
      certificates: status.certificates.map(
        ({
          secretId: _secretId,
          certificatePem: _certificatePem,
          ...certificate
        }) => certificate,
      ),
      desiredRevision: status.desiredRevision
        ? {
            id: status.desiredRevision.id,
            revisionNumber: status.desiredRevision.revisionNumber,
            checksum: status.desiredRevision.checksum,
            createdAt: status.desiredRevision.createdAt,
          }
        : undefined,
      appliedRevision: status.appliedRevision
        ? {
            id: status.appliedRevision.id,
            revisionNumber: status.appliedRevision.revisionNumber,
            checksum: status.appliedRevision.checksum,
            appliedAt: status.appliedRevision.appliedAt,
          }
        : undefined,
    });
  } catch (error) {
    return apiError(error);
  }
}
