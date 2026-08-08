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
): Promise<ReturnType<typeof createServer>> {
  await mkdir(dirname(socketPath), { recursive: true });
  await unlink(socketPath).catch(() => undefined);
  const server = createServer({ allowHalfOpen: true }, (socket) => {
    socket.on("error", () => socket.destroy());
    handleConnection(socket, control);
  });
  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(socketPath, () => {
      server.removeListener("error", reject);
      resolve();
    });
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

function handleConnection(socket: Socket, control: FixedServiceControl): void {
  let buffer = "";
  socket.on("end", () => socket.destroy());
  socket.on("data", async (chunk: Buffer) => {
    buffer += chunk.toString("utf8");
    let newline = buffer.indexOf("\n");
    while (newline >= 0) {
      const line = buffer.slice(0, newline);
      buffer = buffer.slice(newline + 1);
      const response = await handleLine(line, control);
      socket.write(`${JSON.stringify(response)}\n`);
      newline = buffer.indexOf("\n");
    }
  });
}

async function handleLine(
  line: string,
  control: FixedServiceControl,
): Promise<ControlResponse> {
  try {
    return await control.execute(parseControlRequest(line));
  } catch (error) {
    return {
      requestId: "invalid",
      operation: "health",
      ok: false,
      error: error instanceof Error ? error.message : "Invalid control request",
    };
  }
}
