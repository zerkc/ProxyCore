import {
  createCipheriv,
  createDecipheriv,
  randomBytes,
} from "node:crypto";

/**
 * Cluster KEK length in bytes. AES-256 key.
 */
export const CLUSTER_KEK_LENGTH = 32;

const ENVELOPE_PREFIX = "v1.kek";

/**
 * generateClusterKEK returns a fresh 32-byte cluster KEK drawn from a
 * cryptographically secure random source.
 */
export function generateClusterKEK(): Buffer {
  return randomBytes(CLUSTER_KEK_LENGTH);
}

/**
 * isClusterKEKEnvelope reports whether the given string is a cluster-KEK
 * envelope produced by wrapWithClusterKEK. It does not authenticate the
 * envelope's contents.
 */
export function isClusterKEKEnvelope(value: string): boolean {
  return value.startsWith(`${ENVELOPE_PREFIX}:`);
}

/**
 * wrapWithClusterKEK encrypts plaintext with the cluster KEK and returns
 * the envelope "v1.kek:<iv>:<tag>:<ciphertext>" with base64url segments.
 */
export function wrapWithClusterKEK(
  plaintext: Buffer | string,
  kek: Buffer,
): string {
  assertKEK(kek);
  const iv = randomBytes(12);
  const cipher = createCipheriv("aes-256-gcm", kek, iv);
  cipher.setAAD(Buffer.from(ENVELOPE_PREFIX, "utf8"));
  const data = typeof plaintext === "string" ? Buffer.from(plaintext, "utf8") : plaintext;
  const ciphertext = Buffer.concat([cipher.update(data), cipher.final()]);
  const tag = cipher.getAuthTag();
  return [
    ENVELOPE_PREFIX,
    iv.toString("base64url"),
    tag.toString("base64url"),
    ciphertext.toString("base64url"),
  ].join(":");
}

/**
 * unwrapWithClusterKEK decrypts a cluster-KEK envelope and returns the
 * plaintext. Throws when the envelope prefix is wrong, segments do not
 * decode, or the GCM authentication tag does not verify.
 */
export function unwrapWithClusterKEK(envelope: string, kek: Buffer): Buffer {
  assertKEK(kek);
  const segments = envelope.split(":");
  if (segments.length !== 4 || segments[0] !== ENVELOPE_PREFIX) {
    throw new Error("not a cluster-KEK envelope");
  }
  const iv = Buffer.from(segments[1], "base64url");
  const tag = Buffer.from(segments[2], "base64url");
  const ciphertext = Buffer.from(segments[3], "base64url");
  if (iv.length !== 12) throw new Error("invalid iv length");
  if (tag.length !== 16) throw new Error("invalid tag length");
  const decipher = createDecipheriv("aes-256-gcm", kek, iv);
  decipher.setAuthTag(tag);
  decipher.setAAD(Buffer.from(ENVELOPE_PREFIX, "utf8"));
  return Buffer.concat([decipher.update(ciphertext), decipher.final()]);
}

function assertKEK(kek: Buffer): void {
  if (!Buffer.isBuffer(kek) || kek.length !== CLUSTER_KEK_LENGTH) {
    throw new Error(
      `cluster KEK must be a ${CLUSTER_KEK_LENGTH}-byte buffer, got ${kek?.length ?? "undefined"}`,
    );
  }
}
