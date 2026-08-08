import { CONTROL_OPERATIONS, type ControlOperation } from "./index";

export type ControlService = "coredns" | "nginx";

export type ControlRequest = {
  requestId: string;
  operation: ControlOperation;
  service: ControlService;
  revisionId: string;
  checksum: string;
  candidatePath: string;
};

export type ControlResponse = {
  requestId: string;
  ok: boolean;
  operation: ControlOperation;
  output?: unknown;
  error?: string;
};

export function parseControlRequest(line: string): ControlRequest {
  if (line.length > 65_536) {
    throw new Error("Control request is too large");
  }
  const parsed = JSON.parse(line) as Record<string, unknown>;
  if ("command" in parsed || "shell" in parsed || "args" in parsed) {
    throw new Error("Arbitrary command fields are not allowed");
  }
  if (
    typeof parsed.requestId !== "string" ||
    typeof parsed.operation !== "string" ||
    typeof parsed.service !== "string" ||
    typeof parsed.revisionId !== "string" ||
    typeof parsed.checksum !== "string" ||
    typeof parsed.candidatePath !== "string"
  ) {
    throw new Error("Control request fields are invalid");
  }
  if (!CONTROL_OPERATIONS.includes(parsed.operation as ControlOperation)) {
    throw new Error(`Unknown control operation: ${parsed.operation}`);
  }
  if (parsed.service !== "coredns" && parsed.service !== "nginx") {
    throw new Error(`Unknown control service: ${parsed.service}`);
  }
  if (
    !parsed.candidatePath.startsWith("/") ||
    parsed.candidatePath.includes("..") ||
    parsed.candidatePath.includes("\0")
  ) {
    throw new Error("Candidate path is invalid");
  }
  return {
    requestId: parsed.requestId,
    operation: parsed.operation as ControlOperation,
    service: parsed.service as ControlService,
    revisionId: parsed.revisionId,
    checksum: parsed.checksum,
    candidatePath: parsed.candidatePath,
  };
}

export function serializeControlRequest(request: ControlRequest): string {
  return `${JSON.stringify(request)}\n`;
}
