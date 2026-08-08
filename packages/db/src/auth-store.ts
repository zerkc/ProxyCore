import { and, asc, eq, isNull } from "drizzle-orm";
import type {
  AuditEvent,
  AuthStore,
  SessionRecord,
  UserAccount,
} from "@proxycore/domain";
import type { ProxyCoreDatabase } from "./index";
import { auditEvents, sessions, users } from "./schema";

export class PgAuthStore implements AuthStore {
  constructor(private readonly db: ProxyCoreDatabase) {}

  async listUsers(): Promise<UserAccount[]> {
    const rows = await this.db.select().from(users).orderBy(asc(users.username));
    return rows.map(toUser);
  }

  async findUserByUsername(username: string): Promise<UserAccount | undefined> {
    const row = await this.db.query.users.findFirst({
      where: eq(users.username, username),
    });
    return row ? toUser(row) : undefined;
  }

  async findUserById(id: string): Promise<UserAccount | undefined> {
    const row = await this.db.query.users.findFirst({
      where: eq(users.id, id),
    });
    return row ? toUser(row) : undefined;
  }

  async createUser(user: UserAccount): Promise<UserAccount> {
    const [row] = await this.db
      .insert(users)
      .values({
        id: user.id,
        username: user.username,
        passwordHash: user.passwordHash,
        role: user.role,
        active: user.active,
        createdAt: user.createdAt,
        updatedAt: user.updatedAt,
      })
      .returning();
    return toUser(row);
  }

  async updateUser(
    id: string,
    patch: Partial<Pick<UserAccount, "role" | "active" | "passwordHash">>,
  ): Promise<UserAccount> {
    const [row] = await this.db
      .update(users)
      .set({
        ...(patch.role === undefined ? {} : { role: patch.role }),
        ...(patch.active === undefined ? {} : { active: patch.active }),
        ...(patch.passwordHash === undefined ? {} : { passwordHash: patch.passwordHash }),
        updatedAt: new Date(),
      })
      .where(eq(users.id, id))
      .returning();
    if (!row) {
      throw new Error("User not found");
    }
    return toUser(row);
  }

  async deleteUser(id: string): Promise<void> {
    await this.db.delete(users).where(eq(users.id, id));
  }

  async createSession(session: SessionRecord): Promise<SessionRecord> {
    const [row] = await this.db
      .insert(sessions)
      .values({
        id: session.id,
        userId: session.userId,
        tokenHash: session.tokenHash,
        expiresAt: session.expiresAt,
        revokedAt: session.revokedAt,
        lastSeenAt: session.lastSeenAt,
        createdAt: session.createdAt,
      })
      .returning();
    return toSession(row);
  }

  async findSessionByTokenHash(tokenHash: string): Promise<SessionRecord | undefined> {
    const row = await this.db.query.sessions.findFirst({
      where: and(eq(sessions.tokenHash, tokenHash), isNull(sessions.revokedAt)),
    });
    return row ? toSession(row) : undefined;
  }

  async revokeSession(id: string, revokedAt: Date): Promise<void> {
    await this.db.update(sessions).set({ revokedAt }).where(eq(sessions.id, id));
  }

  async touchSession(id: string, lastSeenAt: Date): Promise<void> {
    await this.db.update(sessions).set({ lastSeenAt }).where(eq(sessions.id, id));
  }

  async addAudit(event: AuditEvent): Promise<void> {
    await this.db.insert(auditEvents).values({
      id: event.id,
      actorUserId: event.actorUserId,
      action: event.action,
      resourceType: event.resourceType,
      resourceId: event.resourceId,
      beforeValue: event.beforeValue,
      afterValue: event.afterValue,
      correlationId: event.correlationId,
      result: event.result,
      createdAt: event.createdAt,
    });
  }
}

function toUser(row: typeof users.$inferSelect): UserAccount {
  return {
    id: row.id,
    username: row.username,
    passwordHash: row.passwordHash,
    role: row.role,
    active: row.active,
    createdAt: row.createdAt,
    updatedAt: row.updatedAt,
  };
}

function toSession(row: typeof sessions.$inferSelect): SessionRecord {
  return {
    id: row.id,
    userId: row.userId,
    tokenHash: row.tokenHash,
    expiresAt: row.expiresAt,
    revokedAt: row.revokedAt ?? undefined,
    lastSeenAt: row.lastSeenAt ?? undefined,
    createdAt: row.createdAt,
  };
}
