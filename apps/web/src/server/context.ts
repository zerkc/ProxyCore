import { existsSync } from "node:fs";
import { isIP } from "node:net";
import { networkInterfaces } from "node:os";
import { loadConfig } from "@proxycore/config";
import type { AuthStore, IngressAddresses } from "@proxycore/domain";
import { AuthService } from "@proxycore/domain";
import {
  createDatabase,
  PgAuthStore,
  PgConfigurationStore,
} from "@proxycore/db";
import { InMemoryAuthStore } from "@proxycore/testing";
import {
  InMemoryConfigurationStore,
  type ConfigurationStore,
} from "./configuration";

export type WebContext = {
  auth: AuthService;
  authStore: AuthStore;
  configuration: ConfigurationStore;
  config: ReturnType<typeof loadConfig>;
  defaultIngress: IngressAddresses;
  database?: ReturnType<typeof createDatabase>;
};

const globalContext = globalThis as typeof globalThis & {
  __proxycoreWebContext?: WebContext;
};

export function getWebContext(): WebContext {
  if (!globalContext.__proxycoreWebContext) {
    const config = loadConfig();
    const defaultIngress = configuredOrDetectedIngress(config.proxyIngress, config.nodeEnv);
    const database =
      config.persistenceMode === "postgres"
        ? createDatabase(config.databaseUrl)
        : undefined;
    const authStore: AuthStore = database
      ? new PgAuthStore(database.db)
      : new InMemoryAuthStore();
    globalContext.__proxycoreWebContext = {
      auth: new AuthService(authStore, {
        sessionTtlSeconds: config.sessionTtlSeconds,
      }),
      authStore,
      configuration: database
        ? new PgConfigurationStore(database.db, config.masterKeyBase64, defaultIngress)
        : new InMemoryConfigurationStore(config.masterKeyBase64, defaultIngress),
      config,
      defaultIngress,
      database,
    };
  }
  return globalContext.__proxycoreWebContext;
}

export function resetWebContext(): void {
  globalContext.__proxycoreWebContext = undefined;
}

function configuredOrDetectedIngress(
  configured: IngressAddresses,
  nodeEnv: "development" | "test" | "production",
): IngressAddresses {
  if (configured.ipv4 || configured.ipv6) return configured;
  if (nodeEnv === "test" || isContainerRuntime()) return {};
  return detectLanIngress();
}

function isContainerRuntime(): boolean {
  return process.env.PROXYCORE_RUNTIME === "container" || existsSync("/.dockerenv");
}

function detectLanIngress(): IngressAddresses {
  const candidates = Object.entries(networkInterfaces()).flatMap(([name, addresses]) =>
    (addresses ?? [])
      .filter((address) => String(address.family) === "IPv4")
      .filter((address) => !address.internal && isUsableIpv4(address.address))
      .map((address) => ({
        name,
        address: address.address,
        priority: interfacePriority(name),
        private: isPrivateIpv4(address.address),
      })),
  );
  candidates.sort(
    (left, right) =>
      Number(right.private) - Number(left.private) ||
      left.priority - right.priority ||
      left.name.localeCompare(right.name),
  );
  return candidates[0] ? { ipv4: candidates[0].address } : {};
}

function interfacePriority(name: string): number {
  if (name === "en0") return 0;
  if (name === "eth0") return 1;
  if (name === "wlan0" || name === "en1") return 2;
  return 10;
}

function isUsableIpv4(address: string): boolean {
  return isIP(address) === 4 && address !== "0.0.0.0" && !address.startsWith("169.254.");
}

function isPrivateIpv4(address: string): boolean {
  const octets = address.split(".").map(Number);
  return (
    octets[0] === 10 ||
    (octets[0] === 172 && octets[1] >= 16 && octets[1] <= 31) ||
    (octets[0] === 192 && octets[1] === 168)
  );
}
