import { loadConfig } from "@proxycore/config";
import { AuthService } from "@proxycore/domain";
import { InMemoryAuthStore } from "@proxycore/testing";
import { InMemoryConfigurationStore } from "./configuration";

export type WebContext = {
  auth: AuthService;
  authStore: InMemoryAuthStore;
  configuration: InMemoryConfigurationStore;
  config: ReturnType<typeof loadConfig>;
};

const globalContext = globalThis as typeof globalThis & {
  __proxycoreWebContext?: WebContext;
};

export function getWebContext(): WebContext {
  if (!globalContext.__proxycoreWebContext) {
    const config = loadConfig();
    const authStore = new InMemoryAuthStore();
    globalContext.__proxycoreWebContext = {
      auth: new AuthService(authStore, {
        sessionTtlSeconds: config.sessionTtlSeconds,
      }),
      authStore,
      configuration: new InMemoryConfigurationStore(config.masterKeyBase64),
      config,
    };
  }
  return globalContext.__proxycoreWebContext;
}

export function resetWebContext(): void {
  globalContext.__proxycoreWebContext = undefined;
}
