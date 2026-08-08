import { requestControl } from "../../control/src/transport";
import type {
  ControlRequest,
  ControlResponse,
} from "../../control/src/protocol";
import type { ControlClient } from "./apply";

export class UnixSocketControlClient implements ControlClient {
  constructor(
    private readonly socketPath: string,
    private readonly timeoutMs = 10_000,
  ) {}

  async execute(request: ControlRequest): Promise<ControlResponse> {
    const response = await requestControl(this.socketPath, request, this.timeoutMs);
    if (response.requestId !== request.requestId) {
      throw new Error("Control response request ID does not match");
    }
    if (response.operation !== request.operation) {
      throw new Error("Control response operation does not match");
    }
    return response;
  }
}
