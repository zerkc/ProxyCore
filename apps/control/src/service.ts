import type { ControlOperation } from "./index";
import type { ControlRequest, ControlResponse, ControlService } from "./protocol";

export type ControlHandler = (request: ControlRequest) => Promise<unknown>;
export type FixedHandlers = Partial<
  Record<ControlService, Partial<Record<ControlOperation, ControlHandler>>>
>;

export class FixedServiceControl {
  constructor(private readonly handlers: FixedHandlers) {}

  async execute(request: ControlRequest): Promise<ControlResponse> {
    const handler = this.handlers[request.service]?.[request.operation];
    if (!handler) {
      return {
        requestId: request.requestId,
        operation: request.operation,
        ok: false,
        error: `Operation ${request.operation} is unavailable for ${request.service}`,
      };
    }
    try {
      return {
        requestId: request.requestId,
        operation: request.operation,
        ok: true,
        output: await handler(request),
      };
    } catch (error) {
      return {
        requestId: request.requestId,
        operation: request.operation,
        ok: false,
        error: error instanceof Error ? error.message : "Control operation failed",
      };
    }
  }
}
