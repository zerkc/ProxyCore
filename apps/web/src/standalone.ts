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

type RouteContext = { params: Promise<Record<string, string>> };
type Handler = (
  request: Request,
  context: RouteContext,
) => Response | Promise<Response>;

function asHandler(
  handler: (request: Request, context: RouteContext) => Response | Promise<Response>,
): Handler {
  return (request, context) => handler(request, context);
}

function asSimpleHandler(handler: () => Response | Promise<Response>): Handler {
  return () => handler();
}

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
    body: body && body.length > 0 ? new Uint8Array(body) : undefined,
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
    {
      method: "GET",
      pattern: /^\/api\/health$/,
      keys: [],
      handler: asSimpleHandler(getHealth),
    },
    {
      method: "GET",
      pattern: /^\/api\/status$/,
      keys: [],
      handler: asHandler(getStatus),
    },
    {
      method: "POST",
      pattern: /^\/api\/apply$/,
      keys: [],
      handler: asHandler(postApply),
    },
    {
      method: "GET",
      pattern: /^\/api\/settings$/,
      keys: [],
      handler: asHandler(getSettings),
    },
    {
      method: "PUT",
      pattern: /^\/api\/settings$/,
      keys: [],
      handler: asHandler(putSettings),
    },
    {
      method: "GET",
      pattern: /^\/api\/users$/,
      keys: [],
      handler: asHandler(getUsers),
    },
    {
      method: "POST",
      pattern: /^\/api\/users$/,
      keys: [],
      handler: asHandler(postUsers),
    },
    {
      method: "PATCH",
      pattern: /^\/api\/users\/([^/]+)$/,
      keys: ["userId"],
      handler: asHandler(patchUser as Handler),
    },
    {
      method: "DELETE",
      pattern: /^\/api\/users\/([^/]+)$/,
      keys: ["userId"],
      handler: asHandler(deleteUser as Handler),
    },
    {
      method: "GET",
      pattern: /^\/api\/zones$/,
      keys: [],
      handler: asHandler(getZones),
    },
    {
      method: "POST",
      pattern: /^\/api\/zones$/,
      keys: [],
      handler: asHandler(postZones),
    },
    {
      method: "GET",
      pattern: /^\/api\/zones\/([^/]+)\/records$/,
      keys: ["zoneId"],
      handler: asHandler(getRecords as Handler),
    },
    {
      method: "POST",
      pattern: /^\/api\/zones\/([^/]+)\/records$/,
      keys: ["zoneId"],
      handler: asHandler(postRecords as Handler),
    },
    {
      method: "PATCH",
      pattern: /^\/api\/zones\/([^/]+)\/records\/([^/]+)$/,
      keys: ["zoneId", "recordId"],
      handler: asHandler(patchRecord as Handler),
    },
    {
      method: "GET",
      pattern: /^\/api\/streams$/,
      keys: [],
      handler: asHandler(getStreams),
    },
    {
      method: "POST",
      pattern: /^\/api\/streams$/,
      keys: [],
      handler: asHandler(postStreams),
    },
    {
      method: "PATCH",
      pattern: /^\/api\/streams\/([^/]+)$/,
      keys: ["streamId"],
      handler: asHandler(patchStream as Handler),
    },
    {
      method: "DELETE",
      pattern: /^\/api\/streams\/([^/]+)$/,
      keys: ["streamId"],
      handler: asHandler(deleteStream as Handler),
    },
    {
      method: "GET",
      pattern: /^\/api\/certificates$/,
      keys: [],
      handler: asHandler(getCertificates),
    },
    {
      method: "POST",
      pattern: /^\/api\/certificates$/,
      keys: [],
      handler: asHandler(postCertificates),
    },
    {
      method: "GET",
      pattern: /^\/api\/acme-challenge\/([^/]+)$/,
      keys: ["token"],
      handler: asHandler(getAcmeChallenge as Handler),
    },
    {
      method: "POST",
      pattern: /^\/api\/auth\/bootstrap$/,
      keys: [],
      handler: asHandler(postBootstrap),
    },
    {
      method: "POST",
      pattern: /^\/api\/auth\/login$/,
      keys: [],
      handler: asHandler(postLogin),
    },
    {
      method: "POST",
      pattern: /^\/api\/auth\/logout$/,
      keys: [],
      handler: asHandler(postLogout),
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
