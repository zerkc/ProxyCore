import { randomUUID } from "node:crypto";
import {
  createOpaqueToken,
  hashOpaqueToken,
  hashPassword,
  redactSecrets,
  verifyPassword,
} from "@proxycore/crypto";
import { assertDomain } from "./errors";
import type { Role } from "./model";

export type UserAccount = {
  id: string;
  username: string;
  passwordHash: string;
  role: Role;
  active: boolean;
  createdAt: Date;
  updatedAt: Date;
};

export type PublicUser = Omit<UserAccount, "passwordHash">;

export type SessionRecord = {
  id: string;
  userId: string;
  tokenHash: string;
  expiresAt: Date;
  revokedAt?: Date;
  lastSeenAt?: Date;
  createdAt: Date;
};

export type AuditEvent = {
  id: string;
  actorUserId?: string;
  action: string;
  resourceType: string;
  resourceId?: string;
  beforeValue?: unknown;
  afterValue?: unknown;
  correlationId: string;
  result: "success" | "failure" | "denied";
  createdAt: Date;
};

export interface AuthStore {
  listUsers(): Promise<UserAccount[]>;
  findUserByUsername(username: string): Promise<UserAccount | undefined>;
  findUserById(id: string): Promise<UserAccount | undefined>;
  createUser(user: UserAccount): Promise<UserAccount>;
  updateUser(id: string, patch: Partial<Pick<UserAccount, "role" | "active" | "passwordHash">>): Promise<UserAccount>;
  deleteUser(id: string): Promise<void>;
  createSession(session: SessionRecord): Promise<SessionRecord>;
  findSessionByTokenHash(tokenHash: string): Promise<SessionRecord | undefined>;
  revokeSession(id: string, revokedAt: Date): Promise<void>;
  touchSession(id: string, lastSeenAt: Date): Promise<void>;
  addAudit(event: AuditEvent): Promise<void>;
}

export type AuthSession = {
  token: string;
  userId: string;
  expiresAt: Date;
  user: PublicUser;
};

export type CreateUserInput = {
  username: string;
  password: string;
  role: Role;
};

export class AuthService {
  private readonly now: () => Date;
  private readonly sessionTtlSeconds: number;

  constructor(
    private readonly store: AuthStore,
    options: { sessionTtlSeconds: number; now?: () => Date },
  ) {
    this.sessionTtlSeconds = options.sessionTtlSeconds;
    this.now = options.now ?? (() => new Date());
  }

  async bootstrap(username: string, password: string): Promise<PublicUser> {
    const users = await this.store.listUsers();
    if (users.some((user) => user.active)) {
      await this.audit("bootstrap", "installation", undefined, undefined, "denied");
      throw new Error("Bootstrap is already complete");
    }
    const user = await this.store.createUser(
      await this.buildUser(username, password, "owner"),
    );
    await this.audit("bootstrap", "user", user.id, undefined, "success", { role: user.role });
    return toPublicUser(user);
  }

  async login(username: string, password: string): Promise<AuthSession> {
    const normalizedUsername = normalizeUsername(username);
    const user = await this.store.findUserByUsername(normalizedUsername);
    if (!user || !user.active || !(await verifyPassword(password, user.passwordHash))) {
      await this.audit("login", "user", user?.id, undefined, "failure", {
        username: normalizedUsername,
      });
      throw new Error("Invalid username or password");
    }

    const token = createOpaqueToken();
    const now = this.now();
    const expiresAt = new Date(now.getTime() + this.sessionTtlSeconds * 1_000);
    await this.store.createSession({
      id: randomUUID(),
      userId: user.id,
      tokenHash: hashOpaqueToken(token),
      expiresAt,
      createdAt: now,
    });
    await this.audit("login", "user", user.id, undefined, "success");
    return { token, userId: user.id, expiresAt, user: toPublicUser(user) };
  }

  async authenticate(token: string): Promise<PublicUser> {
    const session = await this.store.findSessionByTokenHash(hashOpaqueToken(token));
    const now = this.now();
    if (!session || session.revokedAt || session.expiresAt <= now) {
      throw new Error("Session is invalid or expired");
    }
    const user = await this.store.findUserById(session.userId);
    if (!user || !user.active) {
      throw new Error("Session user is unavailable");
    }
    await this.store.touchSession(session.id, now);
    return toPublicUser(user);
  }

  async logout(token: string): Promise<void> {
    const session = await this.store.findSessionByTokenHash(hashOpaqueToken(token));
    if (!session || session.revokedAt) {
      return;
    }
    await this.store.revokeSession(session.id, this.now());
    await this.audit("logout", "session", session.id, session.userId, "success");
  }

  async createUser(actorToken: string, input: CreateUserInput): Promise<PublicUser> {
    const actor = await this.requireRole(actorToken, ["owner"]);
    const user = await this.store.createUser(await this.buildUser(input.username, input.password, input.role));
    await this.audit("user.create", "user", user.id, actor.id, "success", { role: user.role });
    return toPublicUser(user);
  }

  async updateUser(
    actorToken: string,
    userId: string,
    patch: Partial<Pick<CreateUserInput, "role" | "password">> & { active?: boolean },
  ): Promise<PublicUser> {
    const actor = await this.requireRole(actorToken, ["owner"]);
    const target = await this.requireUser(userId);
    if (
      target.role === "owner" &&
      (patch.role === "operator" || patch.active === false) &&
      (await this.activeOwnerCount()) <= 1
    ) {
      throw new Error("Cannot remove or demote the last Owner");
    }
    const updated = await this.store.updateUser(userId, {
      role: patch.role,
      active: patch.active,
      passwordHash: patch.password ? await hashPassword(patch.password) : undefined,
    });
    await this.audit("user.update", "user", userId, actor.id, "success", redactSecrets(patch));
    return toPublicUser(updated);
  }

  async deleteUser(actorToken: string, userId: string): Promise<void> {
    const actor = await this.requireRole(actorToken, ["owner"]);
    const target = await this.requireUser(userId);
    if (target.role === "owner" && (await this.activeOwnerCount()) <= 1) {
      throw new Error("Cannot delete the last Owner");
    }
    await this.store.deleteUser(userId);
    await this.audit("user.delete", "user", userId, actor.id, "success");
  }

  async listUsers(actorToken: string): Promise<PublicUser[]> {
    await this.requireRole(actorToken, ["owner"]);
    return (await this.store.listUsers()).map(toPublicUser);
  }

  private async requireRole(token: string, roles: Role[]): Promise<PublicUser> {
    const user = await this.authenticate(token);
    if (!roles.includes(user.role)) {
      await this.audit("authorization.denied", "user", user.id, user.id, "denied", {
        requiredRoles: roles,
      });
      throw new Error("Permission denied");
    }
    return user;
  }

  private async requireUser(userId: string): Promise<UserAccount> {
    const user = await this.store.findUserById(userId);
    if (!user) {
      throw new Error("User not found");
    }
    return user;
  }

  private async activeOwnerCount(): Promise<number> {
    return (await this.store.listUsers()).filter(
      (user) => user.active && user.role === "owner",
    ).length;
  }

  private async buildUser(username: string, password: string, role: Role): Promise<UserAccount> {
    const now = this.now();
    return {
      id: randomUUID(),
      username: normalizeUsername(username),
      passwordHash: await hashPassword(password),
      role,
      active: true,
      createdAt: now,
      updatedAt: now,
    };
  }

  private async audit(
    action: string,
    resourceType: string,
    resourceId: string | undefined,
    actorUserId: string | undefined,
    result: AuditEvent["result"],
    afterValue?: unknown,
  ): Promise<void> {
    await this.store.addAudit({
      id: randomUUID(),
      actorUserId,
      action,
      resourceType,
      resourceId,
      afterValue: afterValue === undefined ? undefined : redactSecrets(afterValue),
      correlationId: randomUUID(),
      result,
      createdAt: this.now(),
    });
  }
}

export function normalizeUsername(username: string): string {
  const normalized = username.trim().toLowerCase();
  assertDomain(
    /^[a-z0-9][a-z0-9._-]{2,63}$/.test(normalized),
    "Username must be 3-64 lowercase letters, digits, dot, underscore, or hyphen",
    "USERNAME_INVALID",
  );
  return normalized;
}

function toPublicUser(user: UserAccount): PublicUser {
  const { passwordHash: _, ...publicUser } = user;
  return publicUser;
}
