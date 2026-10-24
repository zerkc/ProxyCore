// Starts the REAL Bun control server (apps/control startControlServer) with fixed
// handlers for Go client interop tests. Usage: bun control-server.bun.ts <socket>.
// Prints READY once listening and serves until the parent kills the process.
import { FixedServiceControl } from "../../../../../apps/control/src/service";
import { startControlServer } from "../../../../../apps/control/src/transport";

const socketPath = process.argv[2];
const control = new FixedServiceControl({
  coredns: {
    health: async (request) => ({ service: request.service, healthy: true }),
    stage: async (request) => ({
      echo: { ...request },
      text: "héllo <b>&</b>   \"quoted\"",
      list: [1, 2.5, -3, 1e21],
    }),
    reload: async () => null,
  },
  nginx: {
    validate: async () => {
      throw new Error("nginx -t failed: unexpected '}'");
    },
  },
});
await startControlServer(socketPath, control);
console.log("READY");
