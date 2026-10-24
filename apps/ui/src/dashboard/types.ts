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

export type EnrollmentHostnameConfig = {
  configured: boolean;
  hostnames: string[];
};

export type EnrollmentTrustStatus = "unconfigured" | "not-ready" | "ready";

export type EnrollmentTrust = {
  status: EnrollmentTrustStatus;
  configured: boolean;
  ready: boolean;
  certificatePem?: string;
  caCertificatePem?: string;
  caDerSha256?: string;
  expiresAt?: string;
};

export type EnrollmentTrustDownload = {
  blob: Blob;
  filename: string;
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
  /** Running build version. */
  version?: string;
  /** true/false when the updater answered; null when it could not be asked. */
  updating?: boolean | null;
  /** Target version while an update is running. */
  updateTarget?: string;
  /** Running step; only present while updating and when the updater reports it. */
  updateProgress?: UpdateProgress;
};

export type UpdateProgress = {
  /** Updater step name (pull, preflight, restart, verify, bootstrap). */
  step: string;
  /** Steps finished so far. */
  completed: number;
  /** Fixed number of steps in the update. */
  total: number;
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

export type TopologySyncStatus = {
  role: string;
  bound: boolean;
  appliedHash?: string;
  primaryHash?: string;
  outdated: boolean;
  lastSyncAt?: string;
  lastAttemptAt?: string;
  lastError?: string;
  nextAttemptAt?: string;
  consecutiveFailures: number;
  acknowledged: boolean;
  /** True only when the PRIMARY explicitly told this NODE it was unlinked. */
  unlinked: boolean;
  unlinkedAt?: string;
};

export type TopologyNodeStatus = "linked" | "unlinked" | "revoked";

export type TopologyNode = {
  nodeId: string;
  displayName: string;
  status: TopologyNodeStatus;
  ingressIPv4: string;
  ingressIPv6: string;
  lastSeenAt: string | null;
  enrolledAt: string;
  unlinkedAt: string | null;
};

export type TopologyNodeList = {
  /** Server clock used to age lastSeenAt; never compare against the browser clock. */
  serverTime: string;
  nodes: TopologyNode[];
};

export type TopologySyncWarningCode =
  "NODE_OUTDATED" | "NODE_NEVER_SYNCED" | "NODE_SYNC_UNVERIFIED";
