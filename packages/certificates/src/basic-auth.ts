import {
  formatBasicAuthFileLine,
  hashBasicAuthPassword,
} from "@proxycore/crypto";
import type { ProxySettings } from "@proxycore/domain";

type SecretStore = {
  put(purpose: string, plaintext: string): Promise<string>;
  get(id: string): Promise<string | undefined>;
};

export type ProxyBasicAuthInput = {
  username: string;
  password?: string;
  passwordSecretId?: string;
};

export type ProxySettingsInput = Omit<ProxySettings, "basicAuth"> & {
  basicAuth?: ProxyBasicAuthInput;
};

export async function resolveProxySettingsInput(
  input: ProxySettingsInput | undefined,
  options: {
    secretStore?: SecretStore;
    existing?: ProxySettings;
  },
): Promise<ProxySettings | undefined> {
  if (!input) return undefined;
  const { basicAuth, ...rest } = input;
  if (!basicAuth) {
    return { ...rest };
  }

  if (basicAuth.password) {
    if (!options.secretStore) {
      throw new Error("Basic Auth requires a configured master key");
    }
    const passwordHash = hashBasicAuthPassword(basicAuth.password);
    const passwordSecretId = await options.secretStore.put(
      "basic-auth-password",
      passwordHash,
    );
    return {
      ...rest,
      basicAuth: {
        username: basicAuth.username,
        passwordSecretId,
      },
    };
  }

  const passwordSecretId =
    basicAuth.passwordSecretId ?? options.existing?.basicAuth?.passwordSecretId;
  if (!passwordSecretId) {
    throw new Error("Basic Auth password is required");
  }
  if (options.secretStore) {
    const existing = await options.secretStore.get(passwordSecretId);
    if (!existing) {
      throw new Error(`Basic Auth secret not found: ${passwordSecretId}`);
    }
  }

  return {
    ...rest,
    basicAuth: {
      username: basicAuth.username,
      passwordSecretId,
    },
  };
}

export async function materializeBasicAuthFiles(
  records: Array<{ proxy?: ProxySettings }>,
  secretStore: SecretStore,
): Promise<Record<string, string>> {
  const files: Record<string, string> = {};
  for (const record of records) {
    const basicAuth = record.proxy?.basicAuth;
    if (!basicAuth) continue;
    const passwordHash = await secretStore.get(basicAuth.passwordSecretId);
    if (!passwordHash) {
      throw new Error(
        `Basic Auth secret not found: ${basicAuth.passwordSecretId}`,
      );
    }
    files[`basic-auth/${basicAuth.passwordSecretId}`] = formatBasicAuthFileLine(
      basicAuth.username,
      passwordHash,
    );
  }
  return files;
}
