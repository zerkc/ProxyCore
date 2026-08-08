import { eq } from "drizzle-orm";
import { decryptSecret, encryptSecret } from "@proxycore/crypto";
import type { SecretStore } from "@proxycore/certificates";
import type { ProxyCoreDatabase } from "./index";
import { secrets } from "./schema";

export class PgSecretStore implements SecretStore {
  constructor(
    private readonly db: ProxyCoreDatabase,
    private readonly masterKeyBase64: string,
  ) {}

  async put(purpose: string, plaintext: string): Promise<string> {
    const [row] = await this.db
      .insert(secrets)
      .values({
        purpose,
        ciphertext: encryptSecret(plaintext, this.masterKeyBase64),
      })
      .returning({ id: secrets.id });
    return row.id;
  }

  async get(id: string): Promise<string | undefined> {
    const row = await this.db.query.secrets.findFirst({
      where: eq(secrets.id, id),
    });
    return row ? decryptSecret(row.ciphertext, this.masterKeyBase64) : undefined;
  }
}
