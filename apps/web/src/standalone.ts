/**
 * Node configuration API without Next.js.
 * Used transitionally while Go owns SPA + auth; Go proxies /api/* here except
 * health and auth routes already implemented in Go.
 */
import { createServer, type IncomingMessage, type ServerResponse } from "node:http";
import { GET as getHealth } from "./app/api/health/route";
import { GET as getStatus } from "./app/api/status/route";
import { POST as postApply } from "./app/api/apply/route";
import { GET as getSettings, PUT as putSettings } from "./app/api/settings/route";
import { GET as getUsers, POST as postUsers } from "./app/api/users/route";
import {
  PATCH as patchUser,
  DELETE as deleteUser,
} from "./app/api/users/[userId]/route";
import { GET as getZones, POST as postZones } from "./app/api/zones/route";
import {
  GET as getRecords,
  POST as postRecords,
} from "./app/api/zones/[zoneId]/records/route";
import { PATCH as patchRecord } from "./app/api/zones/[zoneId]/records/[recordId]/route";
import { GET as getStreams, POST as postStreams } from "./app/api/streams/route";
import {
  PATCH as patchStream,
  DELETE as deleteStream,
} from "./app/api/streams/[streamId]/route";
import {
  GET as getCertificates,
  POST as postCertificates,
} from "./app/api/certificates/route";
import { GET as getAcmeChallenge } from "./app/api/acme-challenge/[token]/route";
import { POST as postBootstrap } from "./app/api/auth/bootstrap/route";
import { POST as postLogin } from "./app/api/auth/login/route";
import { POST as postLogout } from "./app/api/auth/logout/route";

type Handler = (
  request: Request,
  context?: { params: Promise<Record<string, string>> },
) => Promise<Response>;

const port = Number(process.env.PROXYCORE_NODE_API_PORT ?? 3001);

const server = createServer((req, res) => {
  void dispatch(req, res).catch((error) => {
    res.statusCode = 500;
    res.setHeader("content-type", "application/json");
    res.end(
      JSON.stringify({
        error: error instanceof Error ? error.message : "Request failed",
      }),
    );
  });
});

server.listen(port, "0.0.0.0", () => {
  process.stdout.write(`proxycore-node-api listening on :${port}\n`);
});

async function dispatch(
  req: IncomingMessage,
  res: ServerResponse,
): Promise<void> {
  const host = req.headers.host ?? `127.0.0.1:${port}`;
  const url = new URL(req.url ?? "/", `http://${host}`);
  const body =
    req.method === "GET" || req.method === "HEAD"
      ? undefined
      : await readBody(req);
  const headers = new Headers();
  for (const [key, value] of Object.entries(req.headers)) {
    if (typeof value === "string") headers.set(key, value);
    else if (Array.isArray(value)) headers.set(key, value.join(", "));
  }
  const request = new Request(url, {
    method: req.method,
    headers,
    body,
  });

  const matched = matchRoute(req.method ?? "GET", url.pathname);
  if (!matched) {
    res.statusCode = 404;
    res.setHeader("content-type", "application/json");
    res.end(JSON.stringify({ error: "Not found" }));
    return;
  }

  const response = await matched.handler(request, {
    params: Promise.resolve(matched.params),
  });
  res.statusCode = response.status;
  response.headers.forEach((value, key) => {
    res.setHeader(key, value);
  });
  const buffer = Buffer.from(await response.arrayBuffer());
  res.end(buffer);
}

function matchRoute(
  method: string,
  pathname: string,
): { handler: Handler; params: Record<string, string> } | undefined {
  const routes: Array<{
    method: string;
    pattern: RegExp;
    keys: string[];
    handler: Handler;
  }> = [
    { method: "GET", pattern: /^\/api\/health$/, keys: [], handler: getHealth },
    { method: "GET", pattern: /^\/api\/status$/, keys: [], handler: getStatus },
    { method: "POST", pattern: /^\/api\/apply$/, keys: [], handler: postApply },
    {
      method: "GET",
      pattern: /^\/api\/settings$/,
      keys: [],
      handler: getSettings,
    },
    {
      method: "PUT",
      pattern: /^\/api\/settings$/,
      keys: [],
      handler: putSettings,
    },
    { method: "GET", pattern: /^\/api\/users$/, keys: [], handler: getUsers },
    { method: "POST", pattern: /^\/api\/users$/, keys: [], handler: postUsers },
    {
      method: "PATCH",
      pattern: /^\/api\/users\/([^/]+)$/,
      keys: ["userId"],
      handler: patchUser,
    },
    {
      method: "DELETE",
      pattern: /^\/api\/users\/([^/]+)$/,
      keys: ["userId"],
      handler: deleteUser,
    },
    { method: "GET", pattern: /^\/api\/zones$/, keys: [], handler: getZones },
    { method: "POST", pattern: /^\/api\/zones$/, keys: [], handler: postZones },
    {
      method: "GET",
      pattern: /^\/api\/zones\/([^/]+)\/records$/,
      keys: ["zoneId"],
      handler: getRecords,
    },
    {
      method: "POST",
      pattern: /^\/api\/zones\/([^/]+)\/records$/,
      keys: ["zoneId"],
      handler: postRecords,
    },
    {
      method: "PATCH",
      pattern: /^\/api\/zones\/([^/]+)\/records\/([^/]+)$/,
      keys: ["zoneId", "recordId"],
      handler: patchRecord,
    },
    {
      method: "GET",
      pattern: /^\/api\/streams$/,
      keys: [],
      handler: getStreams,
    },
    {
      method: "POST",
      pattern: /^\/api\/streams$/,
      keys: [],
      handler: postStreams,
    },
    {
      method: "PATCH",
      pattern: /^\/api\/streams\/([^/]+)$/,
      keys: ["streamId"],
      handler: patchStream,
    },
    {
      method: "DELETE",
      pattern: /^\/api\/streams\/([^/]+)$/,
      keys: ["streamId"],
      handler: deleteStream,
    },
    {
      method: "GET",
      pattern: /^\/api\/certificates$/,
      keys: [],
      handler: getCertificates,
    },
    {
      method: "POST",
      pattern: /^\/api\/certificates$/,
      keys: [],
      handler: postCertificates,
    },
    {
      method: "GET",
      pattern: /^\/api\/acme-challenge\/([^/]+)$/,
      keys: ["token"],
      handler: getAcmeChallenge,
    },
    {
      method: "POST",
      pattern: /^\/api\/auth\/bootstrap$/,
      keys: [],
      handler: postBootstrap,
    },
    {
      method: "POST",
      pattern: /^\/api\/auth\/login$/,
      keys: [],
      handler: postLogin,
    },
    {
      method: "POST",
      pattern: /^\/api\/auth\/logout$/,
      keys: [],
      handler: postLogout,
    },
  ];

  for (const route of routes) {
    if (route.method !== method) continue;
    const match = pathname.match(route.pattern);
    if (!match) continue;
    const params: Record<string, string> = {};
    route.keys.forEach((key, index) => {
      params[key] = decodeURIComponent(match[index + 1] ?? "");
    });
    return { handler: route.handler, params };
  }
  return undefined;
}

function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    req.on("data", (chunk) => chunks.push(Buffer.from(chunk)));
    req.on("end", () => resolve(Buffer.concat(chunks)));
    req.on("error", reject);
  });
}
