import {
  boolean,
  integer,
  jsonb,
  pgEnum,
  pgTable,
  text,
  timestamp,
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
    passwordChangeRequired: boolean("password_change_required").notNull().default(false),
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
  }),
);

export const applyJobs = pgTable("apply_jobs", {
  id: uuid("id").defaultRandom().primaryKey(),
  revisionId: uuid("revision_id")
    .notNull()
    .references(() => configRevisions.id),
  actorUserId: uuid("actor_user_id").references(() => users.id),
  target: jobTargetEnum("target").notNull(),
  status: jobStatusEnum("status").notNull().default("queued"),
  correlationId: text("correlation_id").notNull(),
  validationOutput: jsonb("validation_output").$type<unknown>(),
  applyOutput: jsonb("apply_output").$type<unknown>(),
  healthOutput: jsonb("health_output").$type<unknown>(),
  errorMessage: text("error_message"),
  claimedAt: timestamp("claimed_at", { withTimezone: true }),
  startedAt: timestamp("started_at", { withTimezone: true }),
  finishedAt: timestamp("finished_at", { withTimezone: true }),
  createdAt: createdAt(),
});

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
  leadershipGeneration: integer("leadership_generation").notNull().default(1),
  latestKnownGeneration: integer("latest_known_generation").notNull().default(1),
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
    leadershipGeneration: integer("leadership_generation").notNull(),
    snapshotVersion: integer("snapshot_version").notNull(),
    replicationVersion: integer("replication_version").notNull(),
    contentHash: text("content_hash").notNull(),
    revisionId: uuid("revision_id").references(() => configRevisions.id),
    appliedAt: timestamp("applied_at", { withTimezone: true }).notNull().defaultNow(),
    discardedAt: timestamp("discarded_at", { withTimezone: true }),
  },
  (table) => ({
    contentHashIndex: uniqueIndex("applied_snapshots_content_hash_idx").on(
      table.contentHash,
    ),
  }),
);

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
};
