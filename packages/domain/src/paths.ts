import { assertDomain } from "./errors";

export function validateDnsPath(path: string): string {
  const normalized = path.trim();
  assertDomain(normalized.startsWith("/"), "Path must start with /", "PATH_START");
  assertDomain(!/[\r\n?#[\]]/.test(normalized), "Path contains unsupported characters", "PATH_CHARACTERS");
  assertDomain(normalized.length <= 2_048, "Path is too long", "PATH_LENGTH");
  return normalized.length > 1 ? normalized.replace(/\/+$/, "") : normalized;
}
