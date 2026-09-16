import { sql } from "drizzle-orm";
import {
  bigint as bigintColumn,
  boolean,
  check,
  index,
  integer,
  jsonb,
  pgEnum,
  pgTable,
  text,
  timestamp,
  primaryKey,
  uniqueIndex,
  uuid,
} from "drizzle-orm/pg-core";

export const roleEnum = pgEnum("proxycore_role", ["owner", "operator"]);
export const recordTypeEnum = pgEnum("proxycore_record_type", [
  "A",
  "AAAA",
  "CNAME",
  "TXT",
  "MX",
  "SRV",
]);
export const streamProtocolEnum = pgEnum("proxycore_stream_protocol", [
  "tcp",
  "udp",
]);
export const issuerEnum = pgEnum("proxycore_certificate_issuer", [
  "self-signed",
  "uploaded",
  "letsencrypt",
]);
export const challengeEnum = pgEnum("proxycore_certificate_challenge", [
  "none",
  "http-01",
  "dns-01",
]);
export const certificateStatusEnum = pgEnum("proxycore_certificate_status", [
  "pending",
  "issued",
  "active",
  "expired",
  "failed",
]);
export const jobStatusEnum = pgEnum("proxycore_job_status", [
  "queued",
  "validating",
  "applying",
  "applied",
  "failed",
  "rolled-back",
]);
export const jobTargetEnum = pgEnum("proxycore_job_target", [
  "coredns",
  "nginx",
  "combined",
  "certificate",
]);

/**
 * PRIMARY/NODE topology role.
 * Persisted in `installation_identity.role`.
 * Phase 0 introduces the schema; transitions are wired in later work units.
 */
export const topologyRoleEnum = pgEnum("proxycore_topology_role", [
  "standalone-primary",
  "primary",
  "primary-with-nodes",
  "node",
  "stale-primary",
]);
export const enrollmentAttemptStateEnum = pgEnum(
  "proxycore_enrollment_attempt_state",
  [
    "draft",
    "verified",
    "confirmed",
    "exchanged",
    "archived",
    "initial-apply-pending",
    "committed",
    "cancelled",
    "recoverable",
    "failed",
  ],
);
export const syncTriggerEnum = pgEnum("proxycore_sync_trigger", [
  "enrollment",
  "periodic",
  "manual",
  "restart",
]);
export const syncAttemptStatusEnum = pgEnum("proxycore_sync_attempt_status", [
  "queued",
  "running",
  "current",
  "applied",
  "failed",
  "pending-ack",
]);
export const persistenceSourceEnum = pgEnum("proxycore_persistence_source", [
  "ordinary",
  "import",
  "sync",
]);
export const appliedSnapshotStatusEnum = pgEnum(
  "proxycore_applied_snapshot_status",
  ["pending", "applied", "rejected", "rolled-back", "archived"],
);

const createdAt = () =>
  timestamp("created_at", { withTimezone: true }).notNull().defaultNow();
const updatedAt = () =>
  timestamp("updated_at", { withTimezone: true }).notNull().defaultNow();

export const installationSettings = pgTable("installation_settings", {
  id: text("id").primaryKey(),
  ingressIpv4: text("ingress_ipv4"),
  ingressIpv6: text("ingress_ipv6"),
  defaultResolverPool: jsonb("default_resolver_pool").$type<unknown>(),
  forwardingRules: jsonb("forwarding_rules").$type<unknown>(),
  retentionMaxAgeDays: integer("retention_max_age_days").notNull().default(7),
  retentionMaxSizeMb: integer("retention_max_size_mb").notNull().default(50),
  currentDesiredRevisionId: text("current_desired_revision_id"),
  currentAppliedRevisionId: text("current_applied_revision_id"),
  createdAt: createdAt(),
  updatedAt: updatedAt(),
});

export const users = pgTable(
  "users",
  {
    id: uuid("id").defaultRandom().primaryKey(),
    username: text("username").notNull(),
    passwordHash: text("password_hash").notNull(),
    role: roleEnum("role").notNull(),
    active: boolean("active").notNull().default(true),
    passwordChangeRequired: boolean("password_change_required")
      .notNull()
      .default(false),
    createdAt: createdAt(),
    updatedAt: updatedAt(),
  },
  (table) => ({
    usernameIndex: uniqueIndex("users_username_idx").on(table.username),
  }),
);

export const sessions = pgTable(
  "sessions",
  {
    id: uuid("id").defaultRandom().primaryKey(),
    userId: uuid("user_id")
      .notNull()
      .references(() => users.id, { onDelete: "cascade" }),
    tokenHash: text("token_hash").notNull(),
    expiresAt: timestamp("expires_at", { withTimezone: true }).notNull(),
    revokedAt: timestamp("revoked_at", { withTimezone: true }),
    lastSeenAt: timestamp("last_seen_at", { withTimezone: true }),
    createdAt: createdAt(),
  },
  (table) => ({
    tokenIndex: uniqueIndex("sessions_token_hash_idx").on(table.tokenHash),
  }),
);

export const zones = pgTable(
  "zones",
  {
    id: uuid("id").defaultRandom().primaryKey(),
    name: text("name").notNull(),
    enabled: boolean("enabled").notNull().default(true),
    createdAt: createdAt(),
    updatedAt: updatedAt(),
  },
  (table) => ({
    nameIndex: uniqueIndex("zones_name_idx").on(table.name),
  }),
);

export const dnsRecords = pgTable(
  "dns_records",
  {
    id: uuid("id").defaultRandom().primaryKey(),
    zoneId: uuid("zone_id")
      .notNull()
      .references(() => zones.id, { onDelete: "cascade" }),
    name: text("name").notNull(),
    type: recordTypeEnum("type").notNull(),
    value: jsonb("value").notNull().$type<unknown>(),
    ttl: integer("ttl").notNull().default(300),
    enabled: boolean("enabled").notNull().default(true),
    proxied: boolean("proxied").notNull().default(false),
    proxySettings: jsonb("proxy_settings").$type<unknown>(),
    comment: text("comment"),
    createdAt: createdAt(),
    updatedAt: updatedAt(),
  },
  (table) => ({
    zoneNameIndex: uniqueIndex("dns_records_zone_name_id_idx").on(
      table.zoneId,
      table.name,
      table.id,
    ),
  }),
);

export const resolverPools = pgTable(
  "resolver_pools",
  {
    id: uuid("id").defaultRandom().primaryKey(),
    name: text("name").notNull(),
    endpoints: jsonb("endpoints").notNull().$type<unknown>(),
    isDefault: boolean("is_default").notNull().default(false),
    createdAt: createdAt(),
    updatedAt: updatedAt(),
  },
  (table) => ({
    nameIndex: uniqueIndex("resolver_pools_name_idx").on(table.name),
  }),
);

export const forwardingRules = pgTable(
  "forwarding_rules",
  {
    id: uuid("id").defaultRandom().primaryKey(),
    suffix: text("suffix").notNull(),
    poolId: uuid("pool_id")
      .notNull()
      .references(() => resolverPools.id, { onDelete: "cascade" }),
    createdAt: createdAt(),
    updatedAt: updatedAt(),
  },
  (table) => ({
    suffixIndex: uniqueIndex("forwarding_rules_suffix_idx").on(table.suffix),
  }),
);

export const streamRoutes = pgTable(
  "stream_routes",
  {
    id: uuid("id").defaultRandom().primaryKey(),
    enabled: boolean("enabled").notNull().default(true),
    protocol: streamProtocolEnum("protocol").notNull(),
    listenAddress: text("listen_address").notNull(),
    listenPort: integer("listen_port").notNull(),
    upstream: jsonb("upstream").notNull().$type<unknown>(),
    createdAt: createdAt(),
    updatedAt: updatedAt(),
  },
  (table) => ({
    listenerIndex: uniqueIndex("stream_routes_listener_idx").on(
      table.protocol,
      table.listenAddress,
      table.listenPort,
    ),
  }),
);

export const secrets = pgTable("secrets", {
  id: uuid("id").defaultRandom().primaryKey(),
  purpose: text("purpose").notNull(),
  ciphertext: text("ciphertext").notNull(),
  createdAt: createdAt(),
  updatedAt: updatedAt(),
});

/** Long-lived installation CA used to sign auto-generated internal leaf certificates. */
export const internalCa = pgTable("internal_ca", {
  id: text("id").primaryKey(),
  certificatePem: text("certificate_pem").notNull(),
  keySecretId: uuid("key_secret_id")
    .notNull()
    .references(() => secrets.id),
  createdAt: createdAt(),
  updatedAt: updatedAt(),
});

export const certificates = pgTable("certificates", {
  id: uuid("id").defaultRandom().primaryKey(),
  hostnames: jsonb("hostnames").notNull().$type<string[]>(),
  issuer: issuerEnum("issuer").notNull(),
  challenge: challengeEnum("challenge").notNull(),
  environment: text("environment").notNull().default("production"),
  status: certificateStatusEnum("status").notNull().default("pending"),
  expiresAt: timestamp("expires_at", { withTimezone: true }),
  renewAfter: timestamp("renew_after", { withTimezone: true }),
  keySecretId: uuid("key_secret_id").references(() => secrets.id),
  certificatePem: text("certificate_pem"),
  failureReason: text("failure_reason"),
  createdAt: createdAt(),
  updatedAt: updatedAt(),
});

export const providerConnections = pgTable("provider_connections", {
  id: uuid("id").defaultRandom().primaryKey(),
  provider: text("provider").notNull(),
  name: text("name").notNull(),
  secretId: uuid("secret_id")
    .notNull()
    .references(() => secrets.id),
  scope: text("scope").notNull(),
  enabled: boolean("enabled").notNull().default(true),
  createdAt: createdAt(),
  updatedAt: updatedAt(),
});

export const configRevisions = pgTable(
  "config_revisions",
  {
    id: uuid("id").defaultRandom().primaryKey(),
    revisionNumber: integer("revision_number").notNull(),
    checksum: text("checksum").notNull(),
    snapshot: jsonb("snapshot").notNull().$type<unknown>(),
    actorUserId: uuid("actor_user_id").references(() => users.id),
    source: persistenceSourceEnum("source").notNull().default("ordinary"),
    sourcePrimaryId: uuid("source_primary_id"),
    sourceNodeId: uuid("source_node_id"),
    sourceRevisionId: uuid("source_revision_id"),
    snapshotContentHash: text("snapshot_content_hash"),
    snapshotVersion: integer("snapshot_version"),
    replicationVersion: integer("replication_version"),
    leadershipGeneration: bigintColumn("leadership_generation", {
      mode: "number",
    }),
    createdAt: createdAt(),
    appliedAt: timestamp("applied_at", { withTimezone: true }),
  },
  (table) => ({
    numberIndex: uniqueIndex("config_revisions_number_idx").on(
      table.revisionNumber,
    ),
    checksumIndex: uniqueIndex("config_revisions_checksum_idx").on(
      table.checksum,
    ),
    sourceCheck: check(
      "config_revisions_source_check",
      sql`${table.source} in ('ordinary', 'import', 'sync')`,
    ),
  }),
);

export const applyJobs = pgTable(
  "apply_jobs",
  {
    id: uuid("id").defaultRandom().primaryKey(),
    revisionId: uuid("revision_id")
      .notNull()
      .references(() => configRevisions.id),
    actorUserId: uuid("actor_user_id").references(() => users.id),
    target: jobTargetEnum("target").notNull(),
    status: jobStatusEnum("status").notNull().default("queued"),
    source: persistenceSourceEnum("source").notNull().default("ordinary"),
    sourcePrimaryId: uuid("source_primary_id"),
    sourceNodeId: uuid("source_node_id"),
    sourceRevisionId: uuid("source_revision_id"),
    snapshotContentHash: text("snapshot_content_hash"),
    snapshotVersion: integer("snapshot_version"),
    replicationVersion: integer("replication_version"),
    leadershipGeneration: bigintColumn("leadership_generation", {
      mode: "number",
    }),
    correlationId: text("correlation_id").notNull(),
    validationOutput: jsonb("validation_output").$type<unknown>(),
    applyOutput: jsonb("apply_output").$type<unknown>(),
    healthOutput: jsonb("health_output").$type<unknown>(),
    errorMessage: text("error_message"),
    claimedAt: timestamp("claimed_at", { withTimezone: true }),
    startedAt: timestamp("started_at", { withTimezone: true }),
    finishedAt: timestamp("finished_at", { withTimezone: true }),
    createdAt: createdAt(),
  },
  (table) => ({
    sourceCheck: check(
      "apply_jobs_source_check",
      sql`${table.source} in ('ordinary', 'import', 'sync')`,
    ),
  }),
);

export const auditEvents = pgTable("audit_events", {
  id: uuid("id").defaultRandom().primaryKey(),
  actorUserId: uuid("actor_user_id").references(() => users.id),
  action: text("action").notNull(),
  resourceType: text("resource_type").notNull(),
  resourceId: text("resource_id"),
  beforeValue: jsonb("before_value").$type<unknown>(),
  afterValue: jsonb("after_value").$type<unknown>(),
  correlationId: text("correlation_id").notNull(),
  result: text("result").notNull(),
  createdAt: createdAt(),
});

export const healthObservations = pgTable("health_observations", {
  id: uuid("id").defaultRandom().primaryKey(),
  component: text("component").notNull(),
  status: text("status").notNull(),
  details: jsonb("details").$type<unknown>(),
  observedAt: createdAt(),
});

export const operationalArtifacts = pgTable("operational_artifacts", {
  id: uuid("id").defaultRandom().primaryKey(),
  kind: text("kind").notNull(),
  path: text("path").notNull(),
  sizeBytes: integer("size_bytes").notNull(),
  revisionId: uuid("revision_id").references(() => configRevisions.id),
  createdAt: createdAt(),
});

/**
 * Single-row table that records durable installation identity, role, and
 * leadership generation. Phase 0 introduces the schema; the service layer is
 * added in WU 0.3.
 */
export const installationIdentity = pgTable("installation_identity", {
  id: text("id").primaryKey(),
  installationId: uuid("installation_id").notNull(),
  nodeId: uuid("node_id").notNull(),
  role: topologyRoleEnum("role").notNull().default("standalone-primary"),
  leadershipGeneration: bigintColumn("leadership_generation", {
    mode: "number",
  })
    .notNull()
    .default(1),
  latestKnownGeneration: bigintColumn("latest_known_generation", {
    mode: "number",
  })
    .notNull()
    .default(1),
  clusterKeyId: uuid("cluster_key_id"),
  updatedAt: updatedAt(),
});

/**
 * History of cluster key-encryption keys (KEKs). A primary generates a new
 * entry when promoting or rotating; nodes consume the active entry during
 * enrollment. The wrapped_kek column is the cluster KEK encrypted with the
 * local master key.
 */
export const clusterKeys = pgTable("cluster_keys", {
  id: uuid("id").defaultRandom().primaryKey(),
  purpose: text("purpose").notNull(),
  wrappedKek: text("wrapped_kek").notNull(),
  wrappingKeyVersion: integer("wrapping_key_version").notNull(),
  createdAt: createdAt(),
  retiredAt: timestamp("retired_at", { withTimezone: true }),
});

/**
 * Tracks enrollment metadata for the local installation when it acts as a
 * node. The row is a single primary-keyed record identified by the literal
 * "default" id.
 */
export const nodeState = pgTable("node_state", {
  id: text("id").primaryKey(),
  enrolledAt: timestamp("enrolled_at", { withTimezone: true }),
  enrollmentTokenHash: text("enrollment_token_hash"),
  enrollmentPrimaryId: uuid("enrollment_primary_id"),
  lastSeenAt: timestamp("last_seen_at", { withTimezone: true }),
  lastAppliedSnapshotId: uuid("last_applied_snapshot_id"),
  enrollmentAttemptId: uuid("enrollment_attempt_id"),
  primaryUrl: text("primary_url"),
  primaryInstallationId: uuid("primary_installation_id"),
  primaryTlsSpkiSha256: text("primary_tls_spki_sha256"),
  credentialId: uuid("credential_id"),
  syncEnabled: boolean("sync_enabled").notNull().default(false),
  lastAttemptAt: timestamp("last_attempt_at", { withTimezone: true }),
  lastSuccessAt: timestamp("last_success_at", { withTimezone: true }),
  consecutiveFailures: integer("consecutive_failures").notNull().default(0),
  nextAttemptAt: timestamp("next_attempt_at", { withTimezone: true }),
  lastErrorCode: text("last_error_code"),
  updatedAt: updatedAt(),
});

/**
 * Records each snapshot that successfully applied to the local installation.
 * Used by Phase 1 to drive the round-trip and by Phase 0 to retain a
 * previous known-good snapshot for rollback.
 */
export const appliedSnapshots = pgTable(
  "applied_snapshots",
  {
    id: uuid("id").defaultRandom().primaryKey(),
    sourcePrimaryId: uuid("source_primary_id").notNull(),
    leadershipGeneration: bigintColumn("leadership_generation", {
      mode: "number",
    }).notNull(),
    snapshotVersion: integer("snapshot_version").notNull(),
    replicationVersion: integer("replication_version").notNull(),
    contentHash: text("content_hash").notNull(),
    revisionId: uuid("revision_id").references(() => configRevisions.id),
    status: appliedSnapshotStatusEnum("status").notNull().default("applied"),
    applyJobId: uuid("apply_job_id").references(() => applyJobs.id),
    failureCode: text("failure_code"),
    appliedAt: timestamp("applied_at", { withTimezone: true })
      .notNull()
      .defaultNow(),
    discardedAt: timestamp("discarded_at", { withTimezone: true }),
  },
  (table) => ({
    contentHashIndex: uniqueIndex("applied_snapshots_content_hash_idx").on(
      table.contentHash,
    ),
    statusCheck: check(
      "applied_snapshots_status_check",
      sql`${table.status} in ('pending', 'applied', 'rejected', 'rolled-back', 'archived')`,
    ),
  }),
);

export const standaloneArchives = pgTable(
  "standalone_archives",
  {
    id: uuid("id").defaultRandom().primaryKey(),
    snapshotContentHash: text("snapshot_content_hash"),
    revisionId: uuid("revision_id").references(() => configRevisions.id),
    archiveBlob: text("archive_blob").notNull(),
    captureReason: text("capture_reason").notNull(),
    capturedAt: timestamp("captured_at", { withTimezone: true })
      .notNull()
      .defaultNow(),
    retentionStartedAt: timestamp("retention_started_at", {
      withTimezone: true,
    }),
    expiresAt: timestamp("expires_at", { withTimezone: true }),
    restorationEligible: boolean("restoration_eligible")
      .notNull()
      .default(true),
    createdAt: createdAt(),
  },
  (table) => ({
    expiryIndex: index("standalone_archives_expiry_idx").on(table.expiresAt),
  }),
);

export const enrollmentAttempts = pgTable(
  "enrollment_attempts",
  {
    id: uuid("id").defaultRandom().primaryKey(),
    state: enrollmentAttemptStateEnum("state").notNull().default("draft"),
    primaryUrl: text("primary_url").notNull(),
    expectedPrimaryId: uuid("expected_primary_id"),
    verifiedPrimaryId: uuid("verified_primary_id"),
    verifiedPrimaryNodeId: uuid("verified_primary_node_id"),
    verifiedLeadershipGeneration: bigintColumn(
      "verified_leadership_generation",
      { mode: "number" },
    ),
    verifiedPrimaryTlsSpkiSha256: text("verified_primary_tls_spki_sha256"),
    verifiedPrimaryCaFingerprint: text("verified_primary_ca_fingerprint"),
    previewDigest: text("preview_digest"),
    localNodeIp: text("local_node_ip").notNull(),
    archiveId: uuid("archive_id").references(() => standaloneArchives.id),
    ephemeralPrivateKeyWrapped: text("ephemeral_private_key_wrapped").notNull(),
    bootstrapPayload: text("bootstrap_payload"),
    nodeCredentialSecretId: uuid("node_credential_secret_id").references(
      () => secrets.id,
    ),
    clusterKeyId: uuid("cluster_key_id").references(() => clusterKeys.id),
    initialSnapshotHash: text("initial_snapshot_hash"),
    initialSnapshotRevisionId: uuid("initial_snapshot_revision_id").references(
      () => configRevisions.id,
    ),
    initialApplyJobId: uuid("initial_apply_job_id").references(
      () => applyJobs.id,
    ),
    failureCode: text("failure_code"),
    confirmedAt: timestamp("confirmed_at", { withTimezone: true }),
    createdAt: createdAt(),
    updatedAt: updatedAt(),
  },
  (table) => ({
    activeAttemptIndex: uniqueIndex("enrollment_attempts_one_active_idx")
      .on(sql`(1)`)
      .where(sql`${table.state} not in ('committed', 'cancelled', 'failed')`),
    stateCheck: check(
      "enrollment_attempts_state_check",
      sql`${table.state} in ('draft', 'verified', 'confirmed', 'exchanged', 'archived', 'initial-apply-pending', 'committed', 'cancelled', 'recoverable', 'failed')`,
    ),
  }),
);

export const enrollmentTokens = pgTable(
  "enrollment_tokens",
  {
    id: uuid("id").defaultRandom().primaryKey(),
    tokenSelector: text("token_selector").notNull(),
    tokenHash: text("token_hash").notNull(),
    hashVersion: text("hash_version").notNull(),
    createdByUserId: uuid("created_by_user_id")
      .notNull()
      .references(() => users.id),
    createdAt: createdAt(),
    expiresAt: timestamp("expires_at", { withTimezone: true }).notNull(),
    consumedAt: timestamp("consumed_at", { withTimezone: true }),
    consumedByAttemptId: uuid("consumed_by_attempt_id").references(
      () => enrollmentAttempts.id,
    ),
    revokedAt: timestamp("revoked_at", { withTimezone: true }),
  },
  (table) => ({
    selectorIndex: uniqueIndex("enrollment_tokens_selector_idx").on(
      table.tokenSelector,
    ),
    expiryCheck: check(
      "enrollment_tokens_expiry_check",
      sql`${table.expiresAt} > ${table.createdAt}`,
    ),
  }),
);

export const nodeCredentials = pgTable(
  "node_credentials",
  {
    id: uuid("id").defaultRandom().primaryKey(),
    nodeId: uuid("node_id").notNull(),
    credentialHash: text("credential_hash").notNull(),
    hashVersion: text("hash_version").notNull(),
    createdAt: createdAt(),
    lastAuthenticatedAt: timestamp("last_authenticated_at", {
      withTimezone: true,
    }),
    revokedAt: timestamp("revoked_at", { withTimezone: true }),
  },
  (table) => ({
    nodeIndex: uniqueIndex("node_credentials_node_idx").on(table.nodeId),
  }),
);

export const enrolledNodes = pgTable(
  "enrolled_nodes",
  {
    nodeId: uuid("node_id").primaryKey(),
    installationId: uuid("installation_id").notNull(),
    primaryId: uuid("primary_id").notNull(),
    displayName: text("display_name"),
    credentialId: uuid("credential_id")
      .notNull()
      .references(() => nodeCredentials.id),
    enrolledAt: timestamp("enrolled_at", { withTimezone: true })
      .notNull()
      .defaultNow(),
    revokedAt: timestamp("revoked_at", { withTimezone: true }),
    createdByAttemptId: uuid("created_by_attempt_id")
      .notNull()
      .references(() => enrollmentAttempts.id),
  },
  (table) => ({
    installationIndex: uniqueIndex("enrolled_nodes_installation_idx").on(
      table.installationId,
    ),
    credentialIndex: uniqueIndex("enrolled_nodes_credential_idx").on(
      table.credentialId,
    ),
    attemptIndex: uniqueIndex("enrolled_nodes_attempt_unique").on(
      table.createdByAttemptId,
    ),
  }),
);

export const enrollmentGrants = pgTable(
  "enrollment_grants",
  {
    attemptId: uuid("attempt_id")
      .primaryKey()
      .references(() => enrollmentAttempts.id),
    tokenId: uuid("token_id")
      .notNull()
      .unique()
      .references(() => enrollmentTokens.id),
    installationId: uuid("installation_id").notNull(),
    nodeId: uuid("node_id").notNull(),
    primaryId: uuid("primary_id").notNull(),
    primaryGeneration: bigintColumn("primary_generation", {
      mode: "number",
    }).notNull(),
    nodeEphemeralPublicKey: text("node_ephemeral_public_key").notNull(),
    sealedBootstrapPayload: text("sealed_bootstrap_payload").notNull(),
    payloadHash: text("payload_hash").notNull(),
    createdAt: createdAt(),
    expiresAt: timestamp("expires_at", { withTimezone: true }).notNull(),
  },
  (table) => ({
    expiryCheck: check(
      "enrollment_grants_expiry_check",
      sql`${table.expiresAt} > ${table.createdAt}`,
    ),
  }),
);

export const nodeSnapshotAcks = pgTable(
  "node_snapshot_acks",
  {
    nodeId: uuid("node_id")
      .notNull()
      .references(() => enrolledNodes.nodeId),
    contentHash: text("content_hash").notNull(),
    snapshotVersion: integer("snapshot_version").notNull(),
    replicationVersion: integer("replication_version").notNull(),
    revisionId: uuid("revision_id")
      .notNull()
      .references(() => configRevisions.id),
    leadershipGeneration: bigintColumn("leadership_generation", {
      mode: "number",
    }).notNull(),
    appliedAt: timestamp("applied_at", { withTimezone: true }).notNull(),
    receivedAt: timestamp("received_at", { withTimezone: true })
      .notNull()
      .defaultNow(),
  },
  (table) => ({
    primaryKey: primaryKey({ columns: [table.nodeId, table.contentHash] }),
  }),
);

export const syncAttempts = pgTable(
  "sync_attempts",
  {
    id: uuid("id").defaultRandom().primaryKey(),
    nodeId: uuid("node_id").notNull(),
    trigger: syncTriggerEnum("trigger").notNull(),
    status: syncAttemptStatusEnum("status").notNull().default("queued"),
    sourcePrimaryId: uuid("source_primary_id"),
    leadershipGeneration: bigintColumn("leadership_generation", {
      mode: "number",
    }),
    snapshotVersion: integer("snapshot_version"),
    replicationVersion: integer("replication_version"),
    contentHash: text("content_hash"),
    revisionId: uuid("revision_id").references(() => configRevisions.id),
    applyJobId: uuid("apply_job_id").references(() => applyJobs.id),
    resultCode: text("result_code"),
    startedAt: timestamp("started_at", { withTimezone: true }),
    finishedAt: timestamp("finished_at", { withTimezone: true }),
    createdAt: createdAt(),
    updatedAt: updatedAt(),
  },
  (table) => ({
    nodeCreatedIndex: index("sync_attempts_node_created_idx").on(
      table.nodeId,
      table.createdAt,
    ),
    triggerCheck: check(
      "sync_attempts_trigger_check",
      sql`${table.trigger} in ('enrollment', 'periodic', 'manual', 'restart')`,
    ),
  }),
);

export const phase2PersistenceContract = {
  tables: {
    enrollmentTokens: "enrollment_tokens",
    enrolledNodes: "enrolled_nodes",
    nodeCredentials: "node_credentials",
    enrollmentGrants: "enrollment_grants",
    nodeSnapshotAcks: "node_snapshot_acks",
    enrollmentAttempts: "enrollment_attempts",
    syncAttempts: "sync_attempts",
    standaloneArchives: "standalone_archives",
  },
  enrollmentAttemptStates: [
    "draft",
    "verified",
    "confirmed",
    "exchanged",
    "archived",
    "initial-apply-pending",
    "committed",
    "cancelled",
    "recoverable",
    "failed",
  ],
  syncTriggers: ["enrollment", "periodic", "manual", "restart"],
  sources: ["ordinary", "import", "sync"],
  nodeStateColumns: [
    "enrollment_attempt_id",
    "primary_url",
    "primary_installation_id",
    "primary_tls_spki_sha256",
    "credential_id",
    "sync_enabled",
    "last_attempt_at",
    "last_success_at",
    "consecutive_failures",
    "next_attempt_at",
    "last_error_code",
  ],
  attributionColumns: [
    "source",
    "source_primary_id",
    "source_node_id",
    "source_revision_id",
    "snapshot_content_hash",
    "snapshot_version",
    "replication_version",
    "leadership_generation",
  ],
} as const;

export const schema = {
  installationSettings,
  users,
  sessions,
  zones,
  dnsRecords,
  resolverPools,
  forwardingRules,
  streamRoutes,
  secrets,
  internalCa,
  certificates,
  providerConnections,
  configRevisions,
  applyJobs,
  auditEvents,
  healthObservations,
  operationalArtifacts,
  installationIdentity,
  clusterKeys,
  nodeState,
  appliedSnapshots,
  standaloneArchives,
  enrollmentAttempts,
  enrollmentTokens,
  nodeCredentials,
  enrolledNodes,
  enrollmentGrants,
  nodeSnapshotAcks,
  syncAttempts,
};
