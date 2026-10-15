export type EnrollmentTokenCreation = {
  id: string;
  selector: string;
  token: string;
  expiresAt: string;
};

export type EnrollmentTokenDefaults = {
  primaryUrl: string;
  gatewayIPv4: string;
  port: number;
  trustReady: boolean;
  hostCovered: boolean;
};

export type EnrollmentTokenSummary = {
  id: string;
  selector: string;
  createdAt: string;
  expiresAt: string;
  consumedAt?: string | null;
  revokedAt?: string | null;
  attemptSummary?: {
    count: number;
    lastAttemptAt?: string | null;
  };
};

export type EnrollmentIngress = {
  ipv4?: string;
  ipv6?: string;
};

export type EnrollmentEnvelopePreview = {
  ingress: EnrollmentIngress;
  role: string;
  generation: number;
  clusterKeyId?: string;
  contentHash: string;
};

export type EnrollmentNodeOverlay = {
  nodeId: string;
  role: string;
  ingress: EnrollmentIngress;
};

export type DraftPreview = {
  draftId: string;
  envelopePreview: EnrollmentEnvelopePreview;
  nodeLocalOverlay?: EnrollmentNodeOverlay;
  expiresAt: string;
  /** DER SHA-256 of the PRIMARY CA loaded for this draft, when one was loaded. */
  primaryCaFingerprint?: string;
};

export type ConfirmResult = {
  role: string;
  generation: number;
  nodeId: string;
  clusterKeyId?: string;
  archiveId: string;
  applyJobId: string;
};

export type RecoverableState = DraftPreview;

export type TopologyConversionResult = {
  ok: boolean;
  previousRole?: string;
  previousGeneration?: number;
  revokedCredentials?: number;
  nodesMustReenroll?: boolean;
  nextStep?: string;
  warning?: { code: string; message: string };
};
