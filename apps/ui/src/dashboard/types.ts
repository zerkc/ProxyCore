import type { DashboardCertificate } from "./CertificatesView";
import type { EditableRecord } from "./RecordDialog";

export type Zone = {
  id: string;
  name: string;
  records: EditableRecord[];
};

export type StreamRoute = {
  id: string;
  enabled: boolean;
  protocol: "tcp" | "udp";
  listenAddress: string;
  listenPort: number;
  upstream: {
    ip: string;
    port: number;
    protocol: "tcp" | "udp";
  };
};

export type JobRecord = {
  id: string;
  status: string;
  target: string;
  createdAt: string;
  errorMessage?: string | null;
};

export type TopologyRole =
  | "standalone-primary"
  | "primary"
  | "primary-with-nodes"
  | "node"
  | "stale-primary";

export type TopologyIdentity = {
  installationId: string;
  nodeId: string;
  role: TopologyRole;
  leadershipGeneration: number;
  latestKnownGeneration: number;
  stalePrimary: boolean;
  writable: boolean;
};

export type StatusPayload = {
  identity?: TopologyIdentity;
  settings: {
    ingress: { ipv4?: string; ipv6?: string };
    defaultPool?: {
      id: string;
      endpoints: Array<{ host: string; port: number }>;
    };
    forwardingRules: unknown[];
  };
  zones: Zone[];
  streams: StreamRoute[];
  jobs: JobRecord[];
  certificates: DashboardCertificate[];
  desiredRevision?: { revisionNumber: number; checksum: string };
  appliedRevision?: { revisionNumber: number; checksum: string };
};

export type UpdateRelease = {
  version: string;
  tag: string;
  url: string;
  publishedAt: string | null;
};

export type UpdatePayload = {
  status: "current" | "update_available" | "stale" | "unavailable" | "disabled";
  currentVersion: string;
  latest: UpdateRelease | null;
  updateAvailable: boolean;
  checkedAt: string | null;
  updateInProgress: boolean;
  targetVersion?: string;
};
