// Sortable SQL values, not calendar/RFC3339 parsers. Persist as TEXT COLLATE BINARY.
// Seconds are floor Unix seconds; SQL NULL ordering must be handled separately.
export type Timestamp =
  | { kind: "null" }
  | { kind: "negative-infinity" }
  | { kind: "positive-infinity" }
  | { kind: "finite"; seconds: bigint; nanos: number };

const bias = 1n << 63n;
const maxBiasedSeconds = (1n << 64n) - 1n;

export function encodeTimestamp(value: Timestamp): string | null {
  const allowed = value.kind === "finite" ? ["kind", "seconds", "nanos"] : ["kind"];
  if (Object.keys(value).some((key) => !allowed.includes(key))) {
    throw new Error("Contradictory timestamp fields");
  }
  switch (value.kind) {
    case "null":
      return null;
    case "negative-infinity":
      return "0";
    case "positive-infinity":
      return "2";
    case "finite": {
      if (typeof value.seconds !== "bigint" || value.seconds < -bias || value.seconds >= bias) {
        throw new Error("Timestamp seconds must be signed int64 bigint");
      }
      if (!Number.isInteger(value.nanos) || value.nanos < 0 || value.nanos > 999999999) {
        throw new Error("Timestamp nanos outside 0..999999999");
      }
      return `1${(value.seconds + bias).toString().padStart(20, "0")}.${value.nanos.toString().padStart(9, "0")}`;
    }
    default:
      throw new Error("Invalid timestamp variant");
  }
}

export function decodeTimestamp(key: string | null): Timestamp {
  if (key === null) return { kind: "null" };
  if (key === "0") return { kind: "negative-infinity" };
  if (key === "2") return { kind: "positive-infinity" };
  if (typeof key !== "string" || key.length !== 31) {
    throw new Error("Noncanonical timestamp key");
  }
  const parts = /^1([0-9]{20})\.([0-9]{9})$/.exec(key);
  if (!parts) throw new Error("Noncanonical timestamp key");
  const biased = BigInt(parts[1]);
  if (biased > maxBiasedSeconds) throw new Error("Timestamp biased seconds exceed uint64");
  return { kind: "finite", seconds: biased - bias, nanos: Number(parts[2]) };
}
