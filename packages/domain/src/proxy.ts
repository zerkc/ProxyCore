import { isIP } from "node:net";
import { assertDomain } from "./errors";
import type {
  HeaderRule,
  Http3Capabilities,
  PathRule,
  ProxySettings,
  StreamRoute,
} from "./model";
import { validateDnsPath } from "./paths";

export function validateProxySettings(
  settings: ProxySettings,
  capabilities?: Http3Capabilities,
): ProxySettings {
  assertDomain(
    isIP(settings.origin.ip) > 0,
    "Proxy origin must be a literal IP",
    "ORIGIN_IP",
  );
  assertPort(settings.origin.port, "ORIGIN_PORT");
  assertDomain(
    settings.origin.protocol === "http" || settings.origin.protocol === "https",
    "Proxy origin protocol must be http or https",
    "ORIGIN_PROTOCOL",
  );
  assertDomain(
    typeof settings.tlsEnabled === "boolean",
    "TLS setting is required",
    "TLS_REQUIRED",
  );

  if (settings.basicAuth) {
    assertDomain(
      settings.tlsEnabled,
      "Basic Auth requires client TLS",
      "BASIC_AUTH_TLS",
    );
    assertDomain(
      /^[A-Za-z0-9._@+=-]{1,64}$/.test(settings.basicAuth.username),
      "Basic Auth username is invalid",
      "BASIC_AUTH_USERNAME",
    );
    assertDomain(
      /^[A-Za-z0-9_-]{8,128}$/.test(settings.basicAuth.passwordSecretId),
      "Basic Auth secret reference is invalid",
      "BASIC_AUTH_SECRET",
    );
  }

  if (settings.http3) {
    assertDomain(
      capabilities?.http3Module &&
        capabilities.tcp443Published &&
        capabilities.udp443Published,
      "HTTP/3 requires binary support and published TCP/UDP 443",
      "HTTP3_UNAVAILABLE",
    );
    assertDomain(
      settings.tlsEnabled,
      "HTTP/3 requires client TLS",
      "HTTP3_TLS",
    );
  }

  const pathRules = validatePathRules(settings.pathRules ?? []);
  const headers = validateHeaderRules(settings.headers ?? []);
  const nginxDirectives = validateNginxDirectives(settings.nginxDirectives);
  if (settings.cache?.enabled) {
    assertDomain(
      !settings.basicAuth,
      "Authenticated routes cannot enable cache",
      "CACHE_AUTH",
    );
    assertDomain(
      !settings.websocket,
      "WebSocket routes cannot enable cache",
      "CACHE_WEBSOCKET",
    );
  }

  const timeouts = {
    connectSeconds: settings.timeouts?.connectSeconds ?? 5,
    sendReadSeconds: settings.timeouts?.sendReadSeconds ?? 60,
    clientHeaderSeconds: settings.timeouts?.clientHeaderSeconds ?? 15,
    bodyLimitMb: settings.timeouts?.bodyLimitMb ?? 10,
  };
  assertDomain(
    timeouts.connectSeconds > 0 && timeouts.connectSeconds <= 300,
    "Connect timeout is invalid",
  );
  assertDomain(
    timeouts.sendReadSeconds > 0 && timeouts.sendReadSeconds <= 3_600,
    "Send/read timeout is invalid",
  );
  assertDomain(
    timeouts.clientHeaderSeconds > 0 && timeouts.clientHeaderSeconds <= 300,
    "Client timeout is invalid",
  );
  assertDomain(
    timeouts.bodyLimitMb > 0 && timeouts.bodyLimitMb <= 1_024,
    "Body limit is invalid",
  );

  return {
    ...settings,
    http2: settings.http2 ?? settings.tlsEnabled,
    http3: settings.http3 ?? false,
    nginxDirectives,
    headers,
    pathRules,
    timeouts,
  };
}

export function validateNginxDirectives(
  directives: string | undefined,
): string | undefined {
  if (directives === undefined) return undefined;
  assertDomain(
    typeof directives === "string",
    "Nginx directives must be text",
    "NGINX_DIRECTIVES_TYPE",
  );
  const normalized = directives.replace(/\r\n?/g, "\n").trim();
  if (!normalized) return undefined;
  assertDomain(
    normalized.length <= 32_000,
    "Nginx directives are too long",
    "NGINX_DIRECTIVES_LENGTH",
  );
  assertDomain(
    !normalized.includes("\0"),
    "Nginx directives contain an invalid character",
    "NGINX_DIRECTIVES_CHARACTERS",
  );
  assertDomain(
    !containsUnquotedBrace(normalized),
    "Nginx directives cannot contain configuration blocks",
    "NGINX_DIRECTIVES_BLOCK",
  );
  assertDomain(
    !/^\s*(events|http|server|stream|location)\b/im.test(normalized),
    "Nginx block directives are not allowed here",
    "NGINX_DIRECTIVES_CONTEXT",
  );
  return normalized;
}

function containsUnquotedBrace(value: string): boolean {
  let quote: '"' | "'" | undefined;
  let escaped = false;
  for (const character of value) {
    if (escaped) {
      escaped = false;
      continue;
    }
    if (quote && character === "\\") {
      escaped = true;
      continue;
    }
    if (quote) {
      if (character === quote) quote = undefined;
      continue;
    }
    if (character === '"' || character === "'") {
      quote = character;
      continue;
    }
    if (character === "{" || character === "}") return true;
  }
  return false;
}

export function validatePathRules(rules: PathRule[]): PathRule[] {
  const seen = new Set<string>();
  return rules.map((rule) => {
    const pattern = validateDnsPath(rule.pattern);
    const key = `${rule.kind}:${pattern}`;
    assertDomain(
      !seen.has(key),
      `Duplicate path rule: ${pattern}`,
      "PATH_DUPLICATE",
    );
    assertDomain(
      ![...seen].some((existing) => existing.endsWith(`:${pattern}`)),
      `Ambiguous path rule: ${pattern}`,
      "PATH_AMBIGUOUS",
    );
    seen.add(key);
    if (rule.action.type === "redirect") {
      assertDomain(
        [301, 302, 307, 308].includes(rule.action.status),
        "Redirect status is not allowed",
        "REDIRECT_STATUS",
      );
      assertDomain(
        !rule.action.location || !/[\r\n]/.test(rule.action.location),
        "Redirect location cannot contain newlines",
        "REDIRECT_LOCATION",
      );
    } else if (rule.action.rewrite) {
      validateDnsPath(rule.action.rewrite);
    }
    return { ...rule, pattern };
  });
}

export function matchPathRule(
  path: string,
  rules: PathRule[],
): PathRule | undefined {
  const normalizedPath = validateDnsPath(path);
  return [...rules]
    .filter((rule) =>
      rule.kind === "exact"
        ? normalizedPath === rule.pattern
        : normalizedPath === rule.pattern ||
          normalizedPath.startsWith(`${rule.pattern.replace(/\/$/, "")}/`),
    )
    .sort((left, right) => {
      if (left.kind !== right.kind) {
        return left.kind === "exact" ? -1 : 1;
      }
      return right.pattern.length - left.pattern.length;
    })[0];
}

export function validateHeaderRules(headers: HeaderRule[]): HeaderRule[] {
  const seen = new Set<string>();
  return headers.map((header) => {
    assertDomain(
      /^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/.test(header.name),
      `Invalid header name: ${header.name}`,
      "HEADER_NAME",
    );
    assertDomain(
      !/[\r\n]/.test(header.value),
      "Header value cannot contain newlines",
      "HEADER_VALUE",
    );
    const name = header.name.toLowerCase();
    assertDomain(
      !seen.has(name),
      `Duplicate header: ${header.name}`,
      "HEADER_DUPLICATE",
    );
    seen.add(name);
    return { ...header, name };
  });
}

export function validateStreamRoutes(routes: StreamRoute[]): StreamRoute[] {
  const listeners = new Set<string>();
  return routes.map((route) => {
    assertDomain(
      isIP(route.listenAddress) > 0,
      "Stream listen address must be a literal IP",
      "STREAM_LISTEN_IP",
    );
    assertPort(route.listenPort, "STREAM_LISTEN_PORT");
    assertDomain(
      route.upstream.protocol === route.protocol,
      "Stream protocol and upstream protocol must match",
      "STREAM_PROTOCOL",
    );
    assertDomain(
      isIP(route.upstream.ip) > 0,
      "Stream upstream must be a literal IP",
      "STREAM_UPSTREAM_IP",
    );
    assertPort(route.upstream.port, "STREAM_UPSTREAM_PORT");
    const key = `${route.protocol}:${route.listenAddress}:${route.listenPort}`;
    assertDomain(
      !listeners.has(key),
      `Stream listener conflict: ${key}`,
      "STREAM_CONFLICT",
    );
    listeners.add(key);
    return route;
  });
}

function assertPort(port: number, code: string): void {
  assertDomain(
    Number.isInteger(port) && port >= 1 && port <= 65_535,
    "Port is invalid",
    code,
  );
}
