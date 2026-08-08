import { createHash } from "node:crypto";
import {
  validateProxySettings,
  validateStreamRoutes,
  type DnsRecord,
  type Http3Capabilities,
  type PathRule,
  type StreamRoute,
} from "@proxycore/domain";

export type NginxRenderInput = {
  records: DnsRecord[];
  streams: StreamRoute[];
  capabilities: Http3Capabilities;
  candidatePath?: string;
  /** Additional candidate files such as Basic Auth htpasswd and TLS cert/key material. */
  extraFiles?: Record<string, string>;
};

export type NginxCandidate = {
  config: string;
  checksum: string;
  files: Record<string, string>;
};

export function renderNginxCandidate(input: NginxRenderInput): NginxCandidate {
  const streams = validateStreamRoutes(input.streams);
  const records = input.records
    .filter((record) => record.enabled && record.proxied)
    .sort((left, right) => left.name.localeCompare(right.name));
  const candidatePath = input.candidatePath?.replace(/\/+$/, "");
  const servers = records
    .map((record) => renderServer(record, input.capabilities, candidatePath))
    .join("\n\n");
  const streamConfig = streams.length > 0 ? renderStreams(streams) : "";
  const config =
    [
      "events {}",
      "",
      "http {",
      "    include /etc/nginx/mime.types;",
      "    default_type application/octet-stream;",
      "    sendfile on;",
      "    map $http_upgrade $connection_upgrade {",
      "        default upgrade;",
      '        "" close;',
      "    }",
      servers
        .split("\n")
        .map((line) => (line.length > 0 ? `    ${line}` : line))
        .join("\n"),
      "}",
      streamConfig,
    ]
      .filter((section) => section.length > 0)
      .join("\n\n")
      .replaceAll("\n\n\n", "\n\n")
      .trimEnd() + "\n";
  const files: Record<string, string> = {
    "nginx.conf": config,
    ...(input.extraFiles ?? {}),
  };
  const checksum = createHash("sha256")
    .update(
      Object.keys(files)
        .sort()
        .map((path) => `${path}\n${files[path]}`)
        .join("\n"),
    )
    .digest("hex");
  return { config, checksum, files };
}

function renderServer(
  record: DnsRecord,
  capabilities: Http3Capabilities,
  candidatePath: string | undefined,
): string {
  if (!record.proxy) {
    throw new Error(`Proxied record ${record.name} has no proxy settings`);
  }
  const settings = validateProxySettings(record.proxy, capabilities);
  if (settings.tlsEnabled && !settings.certificateId) {
    throw new Error(`TLS proxied record ${record.name} requires a certificate`);
  }

  const origin = `${settings.origin.protocol}://${formatHost(settings.origin.ip)}:${settings.origin.port}`;
  const serverLines = [
    "server {",
    settings.tlsEnabled
      ? `    listen 443 ssl${settings.http2 ? " http2" : ""};`
      : "    listen 80;",
    settings.tlsEnabled
      ? `    listen [::]:443 ssl${settings.http2 ? " http2" : ""};`
      : "    listen [::]:80;",
    ...(settings.http3
      ? [
          "    listen 443 quic reuseport;",
          "    listen [::]:443 quic reuseport;",
          `    add_header Alt-Svc 'h3=":443"; ma=86400' always;`,
        ]
      : []),
    `    server_name ${record.name};`,
    ...(settings.tlsEnabled
      ? [
          `    ssl_certificate ${certificateFilePath(settings.certificateId!, candidatePath, "crt")};`,
          `    ssl_certificate_key ${certificateFilePath(settings.certificateId!, candidatePath, "key")};`,
          "    ssl_protocols TLSv1.2 TLSv1.3;",
        ]
      : []),
    ...(settings.basicAuth
      ? [
          `    auth_basic "ProxyCore";`,
          `    auth_basic_user_file ${basicAuthFilePath(
            settings.basicAuth.passwordSecretId,
            candidatePath,
          )};`,
        ]
      : []),
    ...renderCommonProxyDirectives(settings),
    ...renderPathLocations(settings.pathRules ?? [], origin, settings),
    "}",
  ];

  if (settings.tlsEnabled && (settings.redirectHttpToHttps ?? true)) {
    return [
      "server {",
      "    listen 80;",
      "    listen [::]:80;",
      `    server_name ${record.name};`,
      "    return 308 https://$host$request_uri;",
      "}",
      "",
      serverLines.join("\n"),
    ].join("\n");
  }
  return serverLines.join("\n");
}

function renderCommonProxyDirectives(
  settings: NonNullable<DnsRecord["proxy"]>,
): string[] {
  const customNames = nginxDirectiveNames(settings.nginxDirectives);
  const lines: string[] = [];
  const addDefault = (name: string, line: string) => {
    if (!customNames.has(name)) lines.push(line);
  };

  addDefault("proxy_set_header", "    proxy_set_header Host $host;");
  addDefault(
    "proxy_set_header",
    "    proxy_set_header X-Real-IP $remote_addr;",
  );
  addDefault(
    "proxy_set_header",
    "    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;",
  );
  addDefault(
    "proxy_set_header",
    "    proxy_set_header X-Forwarded-Proto $scheme;",
  );
  addDefault("proxy_http_version", "    proxy_http_version 1.1;");
  addDefault(
    "proxy_connect_timeout",
    `    proxy_connect_timeout ${settings.timeouts?.connectSeconds ?? 5}s;`,
  );
  addDefault(
    "proxy_send_timeout",
    `    proxy_send_timeout ${settings.timeouts?.sendReadSeconds ?? 60}s;`,
  );
  addDefault(
    "proxy_read_timeout",
    `    proxy_read_timeout ${settings.timeouts?.sendReadSeconds ?? 60}s;`,
  );
  addDefault(
    "client_header_timeout",
    `    client_header_timeout ${settings.timeouts?.clientHeaderSeconds ?? 15}s;`,
  );
  addDefault(
    "client_max_body_size",
    `    client_max_body_size ${settings.timeouts?.bodyLimitMb ?? 10}m;`,
  );
  if (settings.origin.protocol === "https") {
    addDefault(
      "proxy_ssl_verify",
      `    proxy_ssl_verify ${settings.backendTlsVerify ? "on" : "off"};`,
    );
  }
  if (settings.websocket) {
    addDefault(
      "proxy_set_header",
      "    proxy_set_header Upgrade $http_upgrade;",
    );
    addDefault(
      "proxy_set_header",
      "    proxy_set_header Connection $connection_upgrade;",
    );
    addDefault("proxy_buffering", "    proxy_buffering off;");
  }
  if (settings.cache?.enabled) {
    addDefault("proxy_cache", "    proxy_cache proxycore_cache;");
  }
  if (!customNames.has("proxy_set_header")) {
    for (const header of settings.headers ?? []) {
      lines.push(
        `    proxy_set_header ${header.name} ${nginxValue(header.value)};`,
      );
    }
  }
  lines.push(...renderNginxDirectives(settings.nginxDirectives));
  return lines;
}

function renderNginxDirectives(directives: string | undefined): string[] {
  return (directives ?? "")
    .split("\n")
    .map((line) => line.trim())
    .filter(Boolean)
    .map((line) => `    ${line}`);
}

function nginxDirectiveNames(directives: string | undefined): Set<string> {
  return new Set(
    (directives ?? "")
      .split("\n")
      .map((line) => line.replace(/#.*/, "").trim())
      .map((line) => line.match(/^([A-Za-z_][A-Za-z0-9_-]*)\b/)?.[1])
      .filter((name): name is string => Boolean(name)),
  );
}

function renderPathLocations(
  rules: PathRule[],
  origin: string,
  settings: NonNullable<DnsRecord["proxy"]>,
): string[] {
  const locations = [...rules].sort((left, right) => {
    if (left.kind !== right.kind) return left.kind === "exact" ? -1 : 1;
    return right.pattern.length - left.pattern.length;
  });
  const hasRootPrefix = locations.some(
    (rule) => rule.kind === "prefix" && rule.pattern === "/",
  );
  if (!hasRootPrefix) {
    locations.push({ kind: "prefix", pattern: "/", action: { type: "proxy" } });
  }

  return locations.map((rule) => {
    const location =
      rule.kind === "exact"
        ? `    location = ${rule.pattern} {`
        : `    location ${rule.pattern} {`;
    if (rule.action.type === "redirect") {
      return [
        location,
        `        return ${rule.action.status} ${nginxValue(rule.action.location ?? "/")};`,
        "    }",
      ].join("\n");
    }
    const passTarget = rule.action.rewrite
      ? `${origin}${rule.action.rewrite}`
      : origin;
    return [
      location,
      `        proxy_pass ${passTarget};`,
      ...(settings.websocket ? ["        proxy_buffering off;"] : []),
      "    }",
    ].join("\n");
  });
}

function renderStreams(streams: StreamRoute[]): string {
  const servers = streams
    .filter((stream) => stream.enabled)
    .map((stream) =>
      [
        "    server {",
        `        listen ${formatHost(stream.listenAddress)}:${stream.listenPort}${stream.protocol === "udp" ? " udp" : ""};`,
        `        proxy_pass ${formatHost(stream.upstream.ip)}:${stream.upstream.port};`,
        ...(stream.protocol === "udp" ? ["        proxy_timeout 60s;"] : []),
        "    }",
      ].join("\n"),
    )
    .join("\n\n");
  return `stream {\n${servers}\n}`;
}

function basicAuthFilePath(
  secretId: string,
  candidatePath: string | undefined,
): string {
  if (!candidatePath) {
    return `/run/proxycore/basic-auth/${secretId}`;
  }
  return `${candidatePath}/basic-auth/${secretId}`;
}

function certificateFilePath(
  certificateId: string,
  candidatePath: string | undefined,
  extension: "crt" | "key",
): string {
  if (!candidatePath) {
    return `/etc/nginx/certs/${certificateId}.${extension}`;
  }
  return `${candidatePath}/certs/${certificateId}.${extension}`;
}

function formatHost(host: string): string {
  return host.includes(":") ? `[${host}]` : host;
}

function nginxValue(value: string): string {
  return /^[a-zA-Z0-9_./:$-]+$/.test(value)
    ? value
    : `"${value.replaceAll("\\", "\\\\").replaceAll('"', '\\"')}"`;
}
