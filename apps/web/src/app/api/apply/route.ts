import { getWebContext } from "../../../server/context";
import { apiError, requireUser } from "../../../server/http";

export async function POST(request: Request) {
  try {
    const { user } = await requireUser(request);
    return Response.json(
      await getWebContext().configuration.createApplyJob(user.id),
      { status: 202 },
    );
  } catch (error) {
    return apiError(error);
  }
}
