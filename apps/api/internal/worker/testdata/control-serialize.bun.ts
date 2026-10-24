// Prints serializeControlRequest(JSON.parse(argv[2])) exactly, for byte-for-byte
// comparison with the Go control client. Usage: bun control-serialize.bun.ts <json>.
import { serializeControlRequest } from "../../../../../apps/control/src/protocol";

process.stdout.write(serializeControlRequest(JSON.parse(process.argv[2])));
