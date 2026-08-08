import { describe, expect, it } from "vitest";
import { parseControlRequest, serializeControlRequest } from "./protocol";

describe("service-control protocol", () => {
  it("accepts fixed operations and serializes JSON-lines safely", () => {
    const request = parseControlRequest(
      serializeControlRequest({
        requestId: "request-1",
        operation: "validate",
        service: "coredns",
        revisionId: "revision-1",
        checksum: "abc",
        candidatePath: "/candidates/revision-1",
      }),
    );

    expect(request.operation).toBe("validate");
    expect(request.service).toBe("coredns");
  });

  it("rejects arbitrary command fields and unknown operations", () => {
    expect(() =>
      parseControlRequest(
        JSON.stringify({
          requestId: "request-1",
          operation: "exec",
          service: "nginx",
          command: "rm -rf /",
        }),
      ),
    ).toThrow(/command|operation/i);
  });
});
