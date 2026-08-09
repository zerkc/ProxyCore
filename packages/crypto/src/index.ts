import {
  createCipheriv,
  createDecipheriv,
  createHash,
  randomBytes,
  scrypt as scryptCallback,
  timingSafeEqual,
} from "node:crypto";
const PASSWORD_N = 16_384;
const PASSWORD_R = 8;
const PASSWORD_P = 1;
const PASSWORD_KEY_LENGTH = 64;
const PASSWORD_SALT_LENGTH = 16;
const MASTER_KEY_LENGTH = 32;

export async function hashPassword(password: string): Promise<string> {
  assertPassword(password);
  const salt = randomBytes(PASSWORD_SALT_LENGTH);
  const derived = await deriveKey(password, salt, PASSWORD_KEY_LENGTH, {
    N: PASSWORD_N,
    r: PASSWORD_R,
    p: PASSWORD_P,
    maxmem: 64 * 1024 * 1024,
  });
  return [
    "scrypt",
    PASSWORD_N,
    PASSWORD_R,
    PASSWORD_P,
    salt.toString("base64url"),
    derived.toString("base64url"),
  ].join("$");
}

export async function verifyPassword(
  password: string,
  encoded: string,
): Promise<boolean> {
  try {
    const [algorithm, n, r, p, saltEncoded, hashEncoded] = encoded.split("$");
    if (algorithm !== "scrypt") {
      return false;
    }
    const salt = Buffer.from(saltEncoded, "base64url");
    const expected = Buffer.from(hashEncoded, "base64url");
    const derived = await deriveKey(password, salt, expected.length, {
      N: Number(n),
      r: Number(r),
      p: Number(p),
      maxmem: 64 * 1024 * 1024,
    });
    return (
      expected.length === derived.length && timingSafeEqual(expected, derived)
    );
  } catch {
    return false;
  }
}

export function createOpaqueToken(): string {
  return randomBytes(32).toString("base64url");
}

export function hashOpaqueToken(token: string): string {
  return createHash("sha256").update(token).digest("hex");
}

export function encryptSecret(
  plaintext: string,
  masterKeyBase64: string,
): string {
  const key = decodeMasterKey(masterKeyBase64);
  const iv = randomBytes(12);
  const cipher = createCipheriv("aes-256-gcm", key, iv);
  const ciphertext = Buffer.concat([
    cipher.update(plaintext, "utf8"),
    cipher.final(),
  ]);
  const tag = cipher.getAuthTag();
  return [
    "v1",
    iv.toString("base64url"),
    tag.toString("base64url"),
    ciphertext.toString("base64url"),
  ].join(":");
}

export function decryptSecret(
  encoded: string,
  masterKeyBase64: string,
): string {
  const [version, ivEncoded, tagEncoded, ciphertextEncoded] =
    encoded.split(":");
  if (version !== "v1" || !ivEncoded || !tagEncoded || !ciphertextEncoded) {
    throw new Error("Unsupported encrypted secret format");
  }
  const key = decodeMasterKey(masterKeyBase64);
  const decipher = createDecipheriv(
    "aes-256-gcm",
    key,
    Buffer.from(ivEncoded, "base64url"),
  );
  decipher.setAuthTag(Buffer.from(tagEncoded, "base64url"));
  return Buffer.concat([
    decipher.update(Buffer.from(ciphertextEncoded, "base64url")),
    decipher.final(),
  ]).toString("utf8");
}

/**
 * Produces an Nginx-compatible htpasswd hash using the `{SHA}` scheme.
 * The plaintext password is never returned; only the hash string is stored.
 */
export function hashBasicAuthPassword(password: string): string {
  assertBasicAuthPassword(password);
  return `{SHA}${createHash("sha1").update(password, "utf8").digest("base64")}`;
}

export function formatBasicAuthFileLine(
  username: string,
  passwordHash: string,
): string {
  assertBasicAuthUsername(username);
  assertDomain(
    passwordHash.startsWith("{SHA}") && passwordHash.length > 6,
    "Basic Auth password hash is invalid",
  );
  return `${username}:${passwordHash}\n`;
}

export function redactSecrets(value: unknown): unknown {
  if (Array.isArray(value)) {
    return value.map(redactSecrets);
  }
  if (!value || typeof value !== "object") {
    return value;
  }
  return Object.fromEntries(
    Object.entries(value).map(([key, entry]) => [
      key,
      /pass(word)?|secret|token|private.?key|ciphertext|authorization/i.test(
        key,
      )
        ? "[REDACTED]"
        : redactSecrets(entry),
    ]),
  );
}

function decodeMasterKey(value: string): Buffer {
  const key = Buffer.from(value, "base64");
  if (key.length !== MASTER_KEY_LENGTH) {
    throw new Error("PROXYCORE_MASTER_KEY_BASE64 must decode to 32 bytes");
  }
  return key;
}

function deriveKey(
  password: string,
  salt: Buffer,
  keyLength: number,
  options: { N: number; r: number; p: number; maxmem: number },
): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    scryptCallback(password, salt, keyLength, options, (error, derived) => {
      if (error) {
        reject(error);
      } else {
        resolve(derived as Buffer);
      }
    });
  });
}

function assertPassword(password: string): void {
  if (password.length < 5) {
    throw new Error("Password must contain at least 5 characters");
  }
}

function assertBasicAuthPassword(password: string): void {
  if (password.length < 8) {
    throw new Error("Basic Auth password must contain at least 8 characters");
  }
  if (/[\r\n]/.test(password)) {
    throw new Error("Basic Auth password cannot contain newlines");
  }
}

function assertBasicAuthUsername(username: string): void {
  if (!/^[A-Za-z0-9._@+=-]{1,64}$/.test(username)) {
    throw new Error("Basic Auth username is invalid");
  }
}

function assertDomain(condition: boolean, message: string): asserts condition {
  if (!condition) {
    throw new Error(message);
  }
}
