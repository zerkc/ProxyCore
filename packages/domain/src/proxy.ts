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
  assertDomain(isIP(settings.origin.ip) > 0, "Proxy origin must be a literal IP", "ORIGIN_IP");
  assertPort(settings.origin.port, "ORIGIN_PORT");
  assertDomain(
    settings.origin.protocol === "http" || settings.origin.protocol === "https",
    "Proxy origin protocol must be http or https",
    "ORIGIN_PROTOCOL",
  );
  assertDomain(typeof settings.tlsEnabled === "boolean", "TLS setting is required", "TLS_REQUIRED");

  if (settings.basicAuth) {
    assertDomain(settings.tlsEnabled, "Basic Auth requires client TLS", "BASIC_AUTH_TLS");
    assertDomain(
      settings.basicAuth.username.length > 0 && settings.basicAuth.passwordSecretId.length > 0,
      "Basic Auth credentials are incomplete",
      "BASIC_AUTH_CREDENTIALS",
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
    assertDomain(settings.tlsEnabled, "HTTP/3 requires client TLS", "HTTP3_TLS");
  }

  const pathRules = validatePathRules(settings.pathRules ?? []);
  const headers = validateHeaderRules(settings.headers ?? []);
  if (settings.cache?.enabled) {
    assertDomain(!settings.basicAuth, "Authenticated routes cannot enable cache", "CACHE_AUTH");
    assertDomain(!settings.websocket, "WebSocket routes cannot enable cache", "CACHE_WEBSOCKET");
  }

  const timeouts = {
    connectSeconds: settings.timeouts?.connectSeconds ?? 5,
    sendReadSeconds: settings.timeouts?.sendReadSeconds ?? 60,
    clientHeaderSeconds: settings.timeouts?.clientHeaderSeconds ?? 15,
    bodyLimitMb: settings.timeouts?.bodyLimitMb ?? 10,
  };
  assertDomain(timeouts.connectSeconds > 0 && timeouts.connectSeconds <= 300, "Connect timeout is invalid");
  assertDomain(timeouts.sendReadSeconds > 0 && timeouts.sendReadSeconds <= 3_600, "Send/read timeout is invalid");
  assertDomain(timeouts.clientHeaderSeconds > 0 && timeouts.clientHeaderSeconds <= 300, "Client timeout is invalid");
  assertDomain(timeouts.bodyLimitMb > 0 && timeouts.bodyLimitMb <= 1_024, "Body limit is invalid");

  return {
    ...settings,
    http2: settings.http2 ?? settings.tlsEnabled,
    http3: settings.http3 ?? false,
    headers,
    pathRules,
    timeouts,
  };
}

export function validatePathRules(rules: PathRule[]): PathRule[] {
  const seen = new Set<string>();
  return rules.map((rule) => {
    const pattern = validateDnsPath(rule.pattern);
    const key = `${rule.kind}:${pattern}`;
    assertDomain(!seen.has(key), `Duplicate path rule: ${pattern}`, "PATH_DUPLICATE");
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

export function matchPathRule(path: string, rules: PathRule[]): PathRule | undefined {
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
    assertDomain(!/[\r\n]/.test(header.value), "Header value cannot contain newlines", "HEADER_VALUE");
    const name = header.name.toLowerCase();
    assertDomain(!seen.has(name), `Duplicate header: ${header.name}`, "HEADER_DUPLICATE");
    seen.add(name);
    return { ...header, name };
  });
}

export function validateStreamRoutes(routes: StreamRoute[]): StreamRoute[] {
  const listeners = new Set<string>();
  return routes.map((route) => {
    assertDomain(isIP(route.listenAddress) > 0, "Stream listen address must be a literal IP", "STREAM_LISTEN_IP");
    assertPort(route.listenPort, "STREAM_LISTEN_PORT");
    assertDomain(
      route.upstream.protocol === route.protocol,
      "Stream protocol and upstream protocol must match",
      "STREAM_PROTOCOL",
    );
    assertDomain(isIP(route.upstream.ip) > 0, "Stream upstream must be a literal IP", "STREAM_UPSTREAM_IP");
    assertPort(route.upstream.port, "STREAM_UPSTREAM_PORT");
    const key = `${route.protocol}:${route.listenAddress}:${route.listenPort}`;
    assertDomain(!listeners.has(key), `Stream listener conflict: ${key}`, "STREAM_CONFLICT");
    listeners.add(key);
    return route;
  });
}

function assertPort(port: number, code: string): void {
  assertDomain(Number.isInteger(port) && port >= 1 && port <= 65_535, "Port is invalid", code);
}
