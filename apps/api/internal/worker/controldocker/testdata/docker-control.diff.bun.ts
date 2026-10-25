// Runs the REAL apps/control/src/docker-control.ts against an in-memory fake of
// dockerode (module stub) and writes {responses, calls, puts} as JSON.
// Usage: bun docker-control.diff.bun.ts <spec.json> <out.json>
import { readFileSync, writeFileSync } from "node:fs";
import { Readable } from "node:stream";

type Spec = {
  candidateRoot: string;
  zonesRoot: string;
  configRoot: string;
  requests: Array<Record<string, string>>;
  files: Record<string, string>;
  execExit: Record<string, number>;
  getErr: string[];
  putFail: boolean;
  restartFail: boolean;
  running: boolean;
};

const [specPath, outPath] = process.argv.slice(2);
const spec: Spec = JSON.parse(readFileSync(specPath, "utf8"));
const files = new Map<string, Buffer>(
  Object.entries(spec.files).map(([k, v]) => [k, Buffer.from(v)]),
);
const calls: string[] = [];
const puts: string[] = [];

function minimalTar(name: string, contents: Buffer): Buffer {
  const header = Buffer.alloc(512);
  Buffer.from(name, "ascii").copy(header, 0);
  header.write(`${contents.length.toString(8).padStart(11, "0")}\0`, 124, 12, "ascii");
  header[156] = 0x30;
  Buffer.from("ustar", "ascii").copy(header, 257);
  Buffer.from("00", "ascii").copy(header, 263);
  header.fill(0x20, 148, 156);
  const checksum = header.reduce((s, v) => s + v, 0).toString(8).padStart(6, "0");
  header.write(checksum, 148, 6, "ascii");
  header[154] = 0;
  header[155] = 0x20;
  const padding = (512 - (contents.length % 512)) % 512;
  return Buffer.concat([header, contents, Buffer.alloc(padding), Buffer.alloc(1024)]);
}

class FakeDocker {
  constructor(_options: unknown) {}
  getContainer(name: string) {
    return {
      inspect: async () => {
        calls.push(`inspect ${name}`);
        return { State: { Running: spec.running } };
      },
      restart: async ({ t }: { t: number }) => {
        calls.push(`restart ${name} t=${t}`);
        if (spec.restartFail) throw new Error("restart failed");
      },
      exec: async ({ Cmd }: { Cmd: string[] }) => {
        const joined = Cmd.join(" ");
        calls.push(`exec ${name} ${joined}`);
        const code = spec.execExit[joined] ?? 0;
        if (code === 0 && Cmd.length === 3 && Cmd[0] === "cp") {
          let data: Buffer | undefined;
          try {
            data = readFileSync(Cmd[1]);
          } catch {
            data = files.get(Cmd[1]);
          }
          if (data) files.set(Cmd[2], data);
        }
        return {
          start: async () => Readable.from([]),
          inspect: async () => ({ ExitCode: code }),
        };
      },
      putArchive: async (archive: Buffer, { path }: { path: string }) => {
        const { extractFirstFileFromTar } = await import(dockerControl);
        const fileName = archive.subarray(0, 100).toString("ascii").replace(/\0.*$/, "");
        const content = extractFirstFileFromTar(archive);
        calls.push(`put ${name} ${path} ${fileName}=${content.toString("utf8")}`);
        puts.push(archive.toString("base64"));
        if (spec.putFail) throw new Error("daemon down");
        files.set(`${path.replace(/\/$/, "")}/${fileName}`, content);
      },
      getArchive: async ({ path }: { path: string }) => {
        calls.push(`get ${name} ${path}`);
        if (spec.getErr.includes(path)) throw new Error("boom");
        const data = files.get(path);
        if (!data) throw new Error("no such file");
        return Readable.from([minimalTar(path.split("/").pop()!, data)]);
      },
    };
  }
}

Bun.plugin({
  name: "dockerode-stub",
  setup(build) {
    build.module("dockerode", () => ({ exports: { default: FakeDocker }, loader: "object" }));
  },
});

const dockerControl = "../../../../../../apps/control/src/docker-control";
const { createDockerServiceControl } = await import(dockerControl);
const control = createDockerServiceControl({
  candidateRoot: spec.candidateRoot,
  corednsZonesRoot: spec.zonesRoot,
  corednsConfigRoot: spec.configRoot,
});
const responses = [];
for (const request of spec.requests) {
  responses.push(await control.execute({ requestId: "req-1", ...request }));
}
writeFileSync(outPath, JSON.stringify({ responses, calls, puts }));
