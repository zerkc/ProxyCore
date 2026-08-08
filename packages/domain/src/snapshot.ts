import { createHash } from "node:crypto";

export type ConfigurationSnapshot = Record<string, unknown>;

export function createSnapshot(value: ConfigurationSnapshot): ConfigurationSnapshot {
  return JSON.parse(stableStringify(value)) as ConfigurationSnapshot;
}

export function checksumSnapshot(value: ConfigurationSnapshot): string {
  return createHash("sha256").update(stableStringify(value)).digest("hex");
}

function stableStringify(value: unknown): string {
  if (value === undefined) {
    return "null";
  }
  if (value === null || typeof value !== "object") {
    return JSON.stringify(value) ?? "null";
  }
  if (value instanceof Date) {
    return JSON.stringify(value.toISOString());
  }
  if (Array.isArray(value)) {
    return `[${value.map(stableStringify).join(",")}]`;
  }
  const object = value as Record<string, unknown>;
  return `{${Object.keys(object)
    .sort()
    .filter((key) => {
      const item = object[key];
      return item !== undefined && typeof item !== "function" && typeof item !== "symbol";
    })
    .map((key) => `${JSON.stringify(key)}:${stableStringify(object[key])}`)
    .join(",")}}`;
}
