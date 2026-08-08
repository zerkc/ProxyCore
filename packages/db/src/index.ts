import { drizzle } from "drizzle-orm/node-postgres";
import { Pool } from "pg";
import { loadConfig } from "@proxycore/config";
import * as schema from "./schema";

export type ProxyCoreDatabase = ReturnType<typeof createDatabase>["db"];

export function createDatabase(databaseUrl = loadConfig().databaseUrl) {
  const pool = new Pool({ connectionString: databaseUrl });
  const db = drizzle({ client: pool, schema });
  return { db, pool };
}

export * from "./schema";
export * from "./ports";
export * from "./auth-store";
