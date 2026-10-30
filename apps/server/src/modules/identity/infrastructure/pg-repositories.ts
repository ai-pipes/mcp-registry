import type { Sql } from '../../../platform/db.ts';
import type {
  PendingLogin,
  PendingLoginRepository,
  SessionRecord,
  SessionRepository,
  TokenRecord,
  TokenRepository,
  UserRepository,
} from '../application/ports.ts';
import type { OidcClaims, Role, User } from '../domain/user.ts';

interface UserRow {
  id: string;
  oidc_sub: string;
  name: string;
  email: string | null;
  groups: string[];
  role: Role;
  disabled: boolean;
  created_at: Date;
  last_login_at: Date | null;
}

const toUser = (r: UserRow): User => ({
  id: r.id,
  oidcSub: r.oidc_sub,
  name: r.name,
  email: r.email,
  groups: r.groups,
  role: r.role,
  disabled: r.disabled,
  createdAt: r.created_at,
  lastLoginAt: r.last_login_at,
});

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
// ids come from the outside (URLs); a malformed one must be "not found", not a 500
const isUuid = (s: string): boolean => UUID.test(s);

export class PgUserRepository implements UserRepository {
  constructor(private readonly sql: Sql) {}

  async findById(id: string): Promise<User | null> {
    if (!isUuid(id)) return null;
    const [r] = await this.sql<UserRow[]>`select * from identity.users where id = ${id}`;
    return r ? toUser(r) : null;
  }

  async findBySub(sub: string): Promise<User | null> {
    const [r] = await this.sql<UserRow[]>`select * from identity.users where oidc_sub = ${sub}`;
    return r ? toUser(r) : null;
  }

  async upsertFromLogin(input: { claims: OidcClaims; role: Role; at: Date }): Promise<User> {
    const { claims, role, at } = input;
    const [r] = await this.sql<UserRow[]>`
      insert into identity.users (oidc_sub, name, email, groups, role, created_at, last_login_at)
      values (${claims.sub}, ${claims.name}, ${claims.email}, ${claims.groups}, ${role}, ${at}, ${at})
      on conflict (oidc_sub) do update
        set name = excluded.name, email = excluded.email, groups = excluded.groups,
            role = excluded.role, last_login_at = excluded.last_login_at
      returning *`;
    if (!r) throw new Error('user upsert returned no row');
    return toUser(r);
  }

  async list(): Promise<User[]> {
    const rows = await this.sql<UserRow[]>`select * from identity.users order by created_at, name`;
    return rows.map(toUser);
  }

  async setDisabled(id: string, disabled: boolean): Promise<boolean> {
    if (!isUuid(id)) return false;
    const r = await this.sql`update identity.users set disabled = ${disabled} where id = ${id}`;
    return r.count > 0;
  }
}

interface TokenRow {
  id: string;
  user_id: string;
  name: string;
  prefix: string;
  hash: string;
  created_at: Date;
  last_used_at: Date | null;
  revoked_at: Date | null;
}

const toToken = (r: TokenRow): TokenRecord => ({
  id: r.id,
  userId: r.user_id,
  name: r.name,
  prefix: r.prefix,
  hash: r.hash,
  createdAt: r.created_at,
  lastUsedAt: r.last_used_at,
  revokedAt: r.revoked_at,
});

export class PgTokenRepository implements TokenRepository {
  constructor(private readonly sql: Sql) {}

  async insert(t: Omit<TokenRecord, 'id' | 'lastUsedAt' | 'revokedAt'>): Promise<TokenRecord> {
    const [r] = await this.sql<TokenRow[]>`
      insert into identity.tokens (user_id, name, prefix, hash, created_at)
      values (${t.userId}, ${t.name}, ${t.prefix}, ${t.hash}, ${t.createdAt}) returning *`;
    if (!r) throw new Error('token insert returned no row');
    return toToken(r);
  }

  async findByHash(hash: string): Promise<TokenRecord | null> {
    const [r] = await this.sql<TokenRow[]>`select * from identity.tokens where hash = ${hash}`;
    return r ? toToken(r) : null;
  }

  async findById(id: string): Promise<TokenRecord | null> {
    if (!isUuid(id)) return null;
    const [r] = await this.sql<TokenRow[]>`select * from identity.tokens where id = ${id}`;
    return r ? toToken(r) : null;
  }

  async listByUser(userId: string): Promise<TokenRecord[]> {
    const rows = await this.sql<TokenRow[]>`
      select * from identity.tokens where user_id = ${userId} order by created_at desc`;
    return rows.map(toToken);
  }

  async revoke(id: string, at: Date): Promise<void> {
    await this.sql`update identity.tokens set revoked_at = ${at}
      where id = ${id} and revoked_at is null`;
  }

  async revokeAllByUser(userId: string, at: Date): Promise<void> {
    await this.sql`update identity.tokens set revoked_at = ${at}
      where user_id = ${userId} and revoked_at is null`;
  }

  async touch(id: string, at: Date): Promise<void> {
    await this.sql`update identity.tokens set last_used_at = ${at} where id = ${id}`;
  }
}

interface SessionRow {
  id_hash: string;
  user_id: string;
  created_at: Date;
  last_seen_at: Date;
  expires_at: Date;
}

export class PgSessionRepository implements SessionRepository {
  constructor(private readonly sql: Sql) {}

  async insert(s: SessionRecord): Promise<void> {
    await this
      .sql`insert into identity.sessions (id_hash, user_id, created_at, last_seen_at, expires_at)
      values (${s.idHash}, ${s.userId}, ${s.createdAt}, ${s.lastSeenAt}, ${s.expiresAt})`;
  }

  async findByHash(idHash: string): Promise<SessionRecord | null> {
    const [r] = await this.sql<SessionRow[]>`
      select * from identity.sessions where id_hash = ${idHash}`;
    return r
      ? {
          idHash: r.id_hash,
          userId: r.user_id,
          createdAt: r.created_at,
          lastSeenAt: r.last_seen_at,
          expiresAt: r.expires_at,
        }
      : null;
  }

  async touch(idHash: string, at: Date): Promise<void> {
    await this.sql`update identity.sessions set last_seen_at = ${at} where id_hash = ${idHash}`;
  }

  async delete(idHash: string): Promise<void> {
    await this.sql`delete from identity.sessions where id_hash = ${idHash}`;
  }

  async deleteByUser(userId: string): Promise<void> {
    await this.sql`delete from identity.sessions where user_id = ${userId}`;
  }

  async deleteDead(c: { expiresAtOrBefore: Date; lastSeenBefore: Date }): Promise<number> {
    const r = await this.sql`delete from identity.sessions
      where expires_at <= ${c.expiresAtOrBefore} or last_seen_at < ${c.lastSeenBefore}`;
    return r.count;
  }
}

interface PendingRow {
  state: string;
  code_verifier: string;
  nonce: string;
  return_to: string;
  expires_at: Date;
}

export class PgPendingLoginRepository implements PendingLoginRepository {
  constructor(private readonly sql: Sql) {}

  async insert(p: PendingLogin): Promise<void> {
    // Abandoned attempts are swept on the way in, so the table cannot grow without bound.
    await this.sql`delete from identity.oidc_pending where expires_at < now() - interval '1 hour'`;
    await this
      .sql`insert into identity.oidc_pending (state, code_verifier, nonce, return_to, expires_at)
      values (${p.state}, ${p.codeVerifier}, ${p.nonce}, ${p.returnTo}, ${p.expiresAt})`;
  }

  async consume(state: string): Promise<PendingLogin | null> {
    const [r] = await this.sql<PendingRow[]>`
      delete from identity.oidc_pending where state = ${state} returning *`;
    return r
      ? {
          state: r.state,
          codeVerifier: r.code_verifier,
          nonce: r.nonce,
          returnTo: r.return_to,
          expiresAt: r.expires_at,
        }
      : null;
  }
}
