import type { MvpRecordType } from "./index";

export type Role = "owner" | "operator";

export type RecordValue = string | MxValue | SrvValue;

export type MxValue = {
  priority: number;
  exchange: string;
};

export type SrvValue = {
  priority: number;
  weight: number;
  port: number;
  target: string;
};

export type UpstreamProtocol = "http" | "https" | "tcp" | "udp";
export type HttpProtocol = "http" | "https";

export type UpstreamTarget = {
  ip: string;
  port: number;
  protocol: UpstreamProtocol;
};

export type PathAction =
  | { type: "proxy"; rewrite?: string }
  | { type: "redirect"; status: 301 | 302 | 307 | 308; location?: string };

export type PathRule = {
  kind: "exact" | "prefix";
  pattern: string;
  action: PathAction;
};

export type HeaderRule = {
  name: string;
  value: string;
};

export type ProxySettings = {
  origin: UpstreamTarget;
  tlsEnabled: boolean;
  redirectHttpToHttps?: boolean;
  certificateId?: string;
  http2?: boolean;
  http3?: boolean;
  /** Raw server-level Nginx directives supplied by the operator. */
  nginxDirectives?: string;
  headers?: HeaderRule[];
  pathRules?: PathRule[];
  basicAuth?: {
    username: string;
    passwordSecretId: string;
  };
  websocket?: boolean;
  cache?: {
    enabled: boolean;
  };
  backendTlsVerify?: boolean;
  timeouts?: {
    connectSeconds?: number;
    sendReadSeconds?: number;
    clientHeaderSeconds?: number;
    bodyLimitMb?: number;
  };
};

export type DnsRecordInput = {
  id: string;
  name: string;
  type: MvpRecordType;
  value: RecordValue;
  ttl?: number;
  enabled: boolean;
  comment?: string;
  proxied: boolean;
  proxy?: ProxySettings;
};

export type DnsRecord = Omit<DnsRecordInput, "ttl" | "name"> & {
  name: string;
  ttl: number;
};

export type IngressAddresses = {
  ipv4?: string;
  ipv6?: string;
};

export type ResolverEndpoint = {
  host: string;
  port: number;
};

export type ResolverPool = {
  id: string;
  endpoints: ResolverEndpoint[];
};

export type ForwardingRule = {
  suffix: string;
  pool: ResolverPool;
};

export type StreamRoute = {
  id: string;
  enabled: boolean;
  protocol: "tcp" | "udp";
  listenAddress: string;
  listenPort: number;
  upstream: UpstreamTarget;
};

export type InstallationSettings = {
  ingress: IngressAddresses;
  defaultPool?: ResolverPool;
  forwardingRules: ForwardingRule[];
  retentionMaxAgeDays: number;
  retentionMaxSizeMb: number;
};

export type ZoneState = {
  id: string;
  name: string;
  enabled: boolean;
  records: DnsRecord[];
};

export type CertificateStatus = {
  id: string;
  hostnames: string[];
  issuer: "self-signed" | "uploaded" | "letsencrypt";
  challenge: "none" | "http-01" | "dns-01";
  environment: string;
  status: "pending" | "issued" | "active" | "expired" | "failed";
  expiresAt?: Date;
  renewAfter?: Date;
  secretId?: string;
  /** Public certificate PEM retained for data-plane materialization. */
  certificatePem?: string;
  failureReason?: string;
};

export type Http3Capabilities = {
  http3Module: boolean;
  tcp443Published: boolean;
  udp443Published: boolean;
};

export type JobStatus =
  "queued" | "validating" | "applying" | "applied" | "failed" | "rolled-back";
