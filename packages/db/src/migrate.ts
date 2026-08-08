import { migrate } from "drizzle-orm/node-postgres/migrator";
import { createDatabase } from "./index";

const { db, pool } = createDatabase();

try {
  await migrate(db, { migrationsFolder: new URL("../migrations", import.meta.url).pathname });
  await pool.end();
} catch (error) {
  await pool.end();
  throw error;
}
