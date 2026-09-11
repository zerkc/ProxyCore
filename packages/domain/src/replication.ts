import { createHash, randomUUID } from "node:crypto";
import type {
  IngressAddresses,
  TopologyRole,
} from "./identity";
import { SnapshotVersionV1, ReplicationVersionV1 } from "./identity";

/**
 * ReplicatedSecret carries a secret-referenced-by-id and its cluster-KEK
 * envelope so a node can decrypt it offline after promotion without
 * contacting the primary.
 */
export interface ReplicatedSecret {
  id: string;
  purpose: string;
  envelope: string;
}

/**
 * ReplicatedOwner carries the durable admin identity that all nodes must
 * hold identically. Plaintext passwords are never replicated.
 */
export interface ReplicatedOwner {
  userId: string;
  username: string;
  passwordHash: string;
  role: "owner" | "operator";
}

/**
 * ReplicatedFields are the values the snapshot carries for every node.
 * The Configuration shape mirrors the legacy ConfigurationSnapshot from
 * apps/api/internal/domain/snapshot.go.
 */
export interface ReplicatedFields {
  configuration: unknown;
  secrets: ReplicatedSecret[];
  owners: ReplicatedOwner[];
}

/**
 * NodeLocalFields are authoritative on the receiving node. They MUST be
 * replaced before a snapshot is applied locally; the validator and the
 * importer both enforce this.
 */
export interface NodeLocalFields {
  ingress: IngressAddresses;
  nodeId: string;
  role: TopologyRole;
  leadershipGeneration: number;
  clusterKeyRef?: string | null;
}

/**
 * TransientFields are authored by the producing primary and describe the
 * envelope itself.
 */
export interface TransientFields {
  snapshotVersion: number;
  replicationVersion: number;
  sourcePrimaryId: string;
  leadershipGeneration: number;
  capturedAt: string;
}

/**
 * Envelope is the outer shape of a replication snapshot.
 */
export interface Envelope {
  transient: TransientFields;
  nodeLocal: NodeLocalFields;
  replicated: ReplicatedFields;
  contentHash: string;
}

/**
 * ClassificationError is thrown when a snapshot places an
 * authoritative-local field inside the replicated payload or vice versa.
 */
export class ClassificationError extends Error {
  constructor(public readonly field: string) {
    super(`snapshot classification error: ${field} placed in wrong field class`);
    this.name = "ClassificationError";
  }
}

/**
 * expectedHash returns the canonical content hash for the envelope. The
 * hash covers a canonical serialization of ReplicatedFields only; the
 * NodeLocal and Transient metadata are not included so a node can
 * re-anchor them at apply time without invalidating the snapshot.
 *
 * The function does NOT classify fields: classification is validate()'s
 * job. expectedHash is a pure serializer so the snapshot can be hashed
 * before other metadata has been populated.
 */
export function expectedHash(env: Envelope): string {
  const canonical = stableStringify(env.replicated);
  return createHash("sha256").update(canonical).digest("hex");
}

/**
 * verifyHash checks that the envelope's declared contentHash matches the
 * canonical content hash. Throws when the hash mismatches.
 */
export function verifyHash(env: Envelope): void {
  const got = expectedHash(env);
  if (got !== env.contentHash) {
    throw new Error(
      `snapshot content hash mismatch: declared ${env.contentHash}, computed ${got}`,
    );
  }
}

/**
 * validate accepts or rejects an envelope purely on its structural shape
 * and declared hash. Field-level validation against the desired state lives
 * in the validator (Phase 1).
 */
export function validate(env: Envelope): void {
  validateClassification(env);
  if (env.transient.snapshotVersion <= 0) {
    throw new Error("invalid transient.snapshotVersion");
  }
  if (env.transient.replicationVersion <= 0) {
    throw new Error("invalid transient.replicationVersion");
  }
  if (!env.transient.capturedAt) {
    throw new Error("missing transient.capturedAt");
  }
  verifyHash(env);
}

/**
 * newEnvelope constructs a fresh envelope with sensible defaults for
 * tests and producers. The Transient.SourcePrimaryId and NodeLocal.NodeId
 * are fresh UUIDs; callers should replace them with the local installation
 * identifiers before publishing.
 */
export function newEnvelope(): Envelope {
  return {
    transient: {
      snapshotVersion: SnapshotVersionV1.value,
      replicationVersion: ReplicationVersionV1.value,
      sourcePrimaryId: randomUUID(),
      leadershipGeneration: 1,
      capturedAt: new Date().toISOString(),
    },
    nodeLocal: {
      ingress: { ipv4: undefined, ipv6: undefined },
      nodeId: randomUUID(),
      role: "standalone-primary",
      leadershipGeneration: 1,
      clusterKeyRef: null,
    },
    replicated: {
      configuration: {},
      secrets: [],
      owners: [],
    },
    contentHash: "",
  };
}

function validateClassification(env: Envelope): void {
  if (!env.transient.sourcePrimaryId) {
    throw new ClassificationError("transient.sourcePrimaryId");
  }
  if (!env.nodeLocal.nodeId) {
    throw new ClassificationError("nodeLocal.nodeId");
  }
  const knownRoles = new Set<TopologyRole>([
    "standalone-primary",
    "primary",
    "primary-with-nodes",
    "node",
    "stale-primary",
  ]);
  if (!knownRoles.has(env.nodeLocal.role)) {
    throw new ClassificationError("nodeLocal.role");
  }
  if (env.replicated.configuration === null || env.replicated.configuration === undefined) {
    throw new ClassificationError("replicated.configuration");
  }
}

function stableStringify(value: unknown): string {
  return JSON.stringify(sortKeys(value));
}

function sortKeys(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(sortKeys);
  if (value && typeof value === "object") {
    const obj = value as Record<string, unknown>;
    const sorted: Record<string, unknown> = {};
    for (const key of Object.keys(obj).sort()) {
      sorted[key] = sortKeys(obj[key]);
    }
    return sorted;
  }
  return value;
}
