import type {
  AuditEvent,
  AuthStore,
  SessionRecord,
  UserAccount,
} from "@proxycore/domain";

export class InMemoryAuthStore implements AuthStore {
  readonly users = new Map<string, UserAccount>();
  readonly sessions = new Map<string, SessionRecord>();
  readonly audits: AuditEvent[] = [];

  async listUsers(): Promise<UserAccount[]> {
    return [...this.users.values()];
  }

  async findUserByUsername(username: string): Promise<UserAccount | undefined> {
    return [...this.users.values()].find((user) => user.username === username);
  }

  async findUserById(id: string): Promise<UserAccount | undefined> {
    return this.users.get(id);
  }

  async createUser(user: UserAccount): Promise<UserAccount> {
    if (await this.findUserByUsername(user.username)) {
      throw new Error("Username already exists");
    }
    this.users.set(user.id, user);
    return user;
  }

  async updateUser(
    id: string,
    patch: Partial<Pick<UserAccount, "role" | "active" | "passwordHash">>,
  ): Promise<UserAccount> {
    const current = this.users.get(id);
    if (!current) {
      throw new Error("User not found");
    }
    const updated = { ...current, ...patch, updatedAt: new Date() };
    this.users.set(id, updated);
    return updated;
  }

  async deleteUser(id: string): Promise<void> {
    this.users.delete(id);
    for (const [sessionId, session] of this.sessions) {
      if (session.userId === id) {
        this.sessions.delete(sessionId);
      }
    }
  }

  async createSession(session: SessionRecord): Promise<SessionRecord> {
    this.sessions.set(session.id, session);
    return session;
  }

  async findSessionByTokenHash(tokenHash: string): Promise<SessionRecord | undefined> {
    return [...this.sessions.values()].find((session) => session.tokenHash === tokenHash);
  }

  async revokeSession(id: string, revokedAt: Date): Promise<void> {
    const session = this.sessions.get(id);
    if (session) {
      this.sessions.set(id, { ...session, revokedAt });
    }
  }

  async touchSession(id: string, lastSeenAt: Date): Promise<void> {
    const session = this.sessions.get(id);
    if (session) {
      this.sessions.set(id, { ...session, lastSeenAt });
    }
  }

  async addAudit(event: AuditEvent): Promise<void> {
    this.audits.push(event);
  }
}
