import { z } from "zod";

const environmentSchema = z.object({
  NODE_ENV: z
    .enum(["development", "test", "production"])
    .default("development"),
  DATABASE_URL: z
    .string()
    .url()
    .default("postgres://proxycore:proxycore@localhost:5432/proxycore"),
  PROXYCORE_PERSISTENCE_MODE: z.enum(["postgres", "memory"]).optional(),
  PROXYCORE_MASTER_KEY_BASE64: z.string().optional(),
  SESSION_COOKIE_NAME: z.string().min(1).default("proxycore_session"),
  SESSION_TTL_SECONDS: z.coerce.number().int().positive().default(28_800),
  PROXY_INGRESS_IPV4: z.string().optional(),
  PROXY_INGRESS_IPV6: z.string().optional(),
  WORKER_SOCKET_PATH: z.string().min(1).default("/run/proxycore/control.sock"),
  CORE_DNS_CONFIG_DIR: z.string().min(1).default("/var/lib/proxycore/coredns"),
  NGINX_CONFIG_DIR: z.string().min(1).default("/var/lib/proxycore/nginx"),
  ACME_DIRECTORY_URL: z
    .string()
    .url()
    .default("https://acme-staging-v02.api.letsencrypt.org/directory"),
  ACME_PRODUCTION_DIRECTORY_URL: z
    .string()
    .url()
    .default("https://acme-v02.api.letsencrypt.org/directory"),
});

export type AppConfig = {
  nodeEnv: "development" | "test" | "production";
  databaseUrl: string;
  persistenceMode: "postgres" | "memory";
  masterKeyBase64?: string;
  sessionCookieName: string;
  sessionTtlSeconds: number;
  proxyIngress: {
    ipv4?: string;
    ipv6?: string;
  };
  workerSocketPath: string;
  coreDnsConfigDir: string;
  nginxConfigDir: string;
  acmeDirectoryUrl: string;
  acmeProductionDirectoryUrl: string;
};

export function loadConfig(
  environment: Record<string, string | undefined> = process.env,
): AppConfig {
  const parsed = environmentSchema.parse(environment);
  return {
    nodeEnv: parsed.NODE_ENV,
    databaseUrl: parsed.DATABASE_URL,
    persistenceMode:
      parsed.PROXYCORE_PERSISTENCE_MODE ??
      (parsed.NODE_ENV === "test" ? "memory" : "postgres"),
    masterKeyBase64: parsed.PROXYCORE_MASTER_KEY_BASE64,
    sessionCookieName: parsed.SESSION_COOKIE_NAME,
    sessionTtlSeconds: parsed.SESSION_TTL_SECONDS,
    proxyIngress: {
      ipv4: parsed.PROXY_INGRESS_IPV4,
      ipv6: parsed.PROXY_INGRESS_IPV6,
    },
    workerSocketPath: parsed.WORKER_SOCKET_PATH,
    coreDnsConfigDir: parsed.CORE_DNS_CONFIG_DIR,
    nginxConfigDir: parsed.NGINX_CONFIG_DIR,
    acmeDirectoryUrl: parsed.ACME_DIRECTORY_URL,
    acmeProductionDirectoryUrl: parsed.ACME_PRODUCTION_DIRECTORY_URL,
  };
}
