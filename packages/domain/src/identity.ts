import { randomUUID } from "node:crypto";

/**
 * TopologyRole is the durable PRIMARY/NODE role of a ProxyCore installation.
 *
 * Distinct from the admin Role (owner/operator) exported from `./model`.
 */
export type TopologyRole =
  | "standalone-primary"
  | "primary"
  | "primary-with-nodes"
  | "node"
  | "stale-primary";

export const TopologyRoleStandalone: TopologyRole = "standalone-primary";
export const TopologyRolePrimary: TopologyRole = "primary";
export const TopologyRolePrimaryWithNodes: TopologyRole = "primary-with-nodes";
export const TopologyRoleNode: TopologyRole = "node";
export const TopologyRoleStalePrimary: TopologyRole = "stale-primary";

const knownTopologyRoles: ReadonlySet<TopologyRole> = new Set([
  TopologyRoleStandalone,
  TopologyRolePrimary,
  TopologyRolePrimaryWithNodes,
  TopologyRoleNode,
  TopologyRoleStalePrimary,
]);

export function isTopologyRole(value: unknown): value is TopologyRole {
  return (
    typeof value === "string" && knownTopologyRoles.has(value as TopologyRole)
  );
}

export class InstallationID {
  readonly value: string;

  constructor(value: string) {
    this.value = value;
  }

  toString(): string {
    return this.value;
  }

  isValid(): boolean {
    return isUuidV4(this.value);
  }
}

export function newInstallationID(): InstallationID {
  return new InstallationID(randomUUID());
}

export class NodeID {
  readonly value: string;

  constructor(value: string) {
    this.value = value;
  }

  toString(): string {
    return this.value;
  }

  isValid(): boolean {
    return isUuidV4(this.value);
  }
}

export function newNodeID(): NodeID {
  return new NodeID(randomUUID());
}

const MAX_GENERATION = Number.MAX_SAFE_INTEGER;

export class LeadershipGeneration {
  readonly value: number;

  constructor(value: number) {
    if (!Number.isInteger(value) || value < 0 || value > MAX_GENERATION) {
      throw new Error(
        `LeadershipGeneration must be an integer in [0, ${MAX_GENERATION}]`,
      );
    }
    this.value = value;
  }

  next(): LeadershipGeneration {
    if (this.value === MAX_GENERATION) {
      throw new Error("LeadershipGeneration overflow");
    }
    return new LeadershipGeneration(this.value + 1);
  }

  isLessThan(other: LeadershipGeneration): boolean {
    return this.value < other.value;
  }

  isGreaterThan(other: LeadershipGeneration): boolean {
    return this.value > other.value;
  }

  equals(other: LeadershipGeneration): boolean {
    return this.value === other.value;
  }
}

export class SnapshotVersion {
  readonly value: number;

  constructor(value: number) {
    if (!Number.isInteger(value) || value < 0) {
      throw new Error("SnapshotVersion must be a non-negative integer");
    }
    this.value = value;
  }

  isValid(): boolean {
    return this.value !== 0;
  }
}

export const SnapshotVersionV1 = new SnapshotVersion(1);

export class ReplicationVersion {
  readonly value: number;

  constructor(value: number) {
    if (!Number.isInteger(value) || value < 0) {
      throw new Error("ReplicationVersion must be a non-negative integer");
    }
    this.value = value;
  }

  isValid(): boolean {
    return this.value !== 0;
  }
}

export const ReplicationVersionV1 = new ReplicationVersion(1);

function isUuidV4(value: string): boolean {
  if (value.length !== 36) return false;
  if (
    value[8] !== "-" ||
    value[13] !== "-" ||
    value[18] !== "-" ||
    value[23] !== "-"
  ) {
    return false;
  }
  for (let i = 0; i < 36; i++) {
    if (i === 8 || i === 13 || i === 18 || i === 23) continue;
    const code = value.charCodeAt(i);
    const isDigit = code >= 0x30 && code <= 0x39;
    const isLowerHex = code >= 0x61 && code <= 0x66;
    if (!isDigit && !isLowerHex) return false;
  }
  if (value[14] !== "4") return false;
  switch (value[19]) {
    case "8":
    case "9":
    case "a":
    case "b":
      return true;
    default:
      return false;
  }
}
