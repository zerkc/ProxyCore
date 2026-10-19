import { createConnection, createServer, type Socket } from "node:net";
import { mkdir, unlink } from "node:fs/promises";
import { dirname } from "node:path";
import {
  parseControlRequest,
  serializeControlRequest,
  type ControlRequest,
  type ControlResponse,
} from "./protocol";
import { FixedServiceControl } from "./service";

export async function startControlServer(
  socketPath: string,
  control: FixedServiceControl,
  log?: (line: string) => void,
): Promise<ReturnType<typeof createServer>> {
  await mkdir(dirname(socketPath), { recursive: true }).catch(
    (error: unknown) => {
      log?.("control socket failed reason=setup_failed stage=mkdir");
      throw error;
    },
  );
  await unlink(socketPath).catch(() => undefined);
  const server = createServer({ allowHalfOpen: true }, (socket) => {
    socket.on("error", () => {
      log?.("control socket failed reason=socket_error");
      socket.destroy();
    });
    handleConnection(socket, control, log);
  });
  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(socketPath, () => {
      server.removeListener("error", reject);
      resolve();
    });
  }).catch((error: unknown) => {
    log?.("control socket failed reason=setup_failed stage=listen");
    throw error;
  });
  return server;
}

export function requestControl(
  socketPath: string,
  request: ControlRequest,
  timeoutMs = 10_000,
): Promise<ControlResponse> {
  return new Promise((resolve, reject) => {
    const socket = createConnection(socketPath);
    let buffer = "";
    const timeout = setTimeout(() => {
      socket.destroy();
      reject(new Error("Control request timed out"));
    }, timeoutMs);

    socket.on("data", (chunk: Buffer) => {
      buffer += chunk.toString("utf8");
      const newline = buffer.indexOf("\n");
      if (newline < 0) return;
      clearTimeout(timeout);
      socket.end();
      resolve(JSON.parse(buffer.slice(0, newline)) as ControlResponse);
    });
    socket.on("error", (error) => {
      clearTimeout(timeout);
      reject(error);
    });
    socket.write(serializeControlRequest(request));
  });
}

function handleConnection(
  socket: Socket,
  control: FixedServiceControl,
  log?: (line: string) => void,
): void {
  let buffer = "";
  socket.on("end", () => socket.destroy());
  socket.on("data", async (chunk: Buffer) => {
    buffer += chunk.toString("utf8");
    let newline = buffer.indexOf("\n");
    while (newline >= 0) {
      const line = buffer.slice(0, newline);
      buffer = buffer.slice(newline + 1);
      const response = await handleLine(line, control, log);
      socket.write(`${JSON.stringify(response)}\n`);
      newline = buffer.indexOf("\n");
    }
  });
}

async function handleLine(
  line: string,
  control: FixedServiceControl,
  log?: (line: string) => void,
): Promise<ControlResponse> {
  try {
    const request = parseControlRequest(line);
    const response = await control.execute(request);
    if (!response.ok) {
      log?.(
        `control request failed request=${safeLogId(request.requestId)} operation=${request.operation} service=${request.service} reason=operation_failed`,
      );
    }
    return response;
  } catch (error) {
    log?.(
      `control request failed reason=${error instanceof SyntaxError ? "invalid_json" : "invalid_request"}`,
    );
    return {
      requestId: "invalid",
      operation: "health",
      ok: false,
      error: error instanceof Error ? error.message : "Invalid control request",
    };
  }
}

function safeLogId(value: string): string {
  return /^[A-Za-z0-9_-]{1,80}$/.test(value) ? value : "[redacted]";
}
