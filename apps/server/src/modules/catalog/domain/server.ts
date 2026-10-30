import { type DomainEvent, err, ok, type Result } from '../../../shared-kernel/index.ts';
import { Access, type ServerStatus } from './access.ts';
import { AuthMode } from './auth-mode.ts';
import { CatalogError } from './errors.ts';
import { SERVER_CHANGED, serverChanged } from './events.ts';
import { ServerSlug } from './server-slug.ts';
import type { SyncRecord } from './sync.ts';

const DEFAULT_TIMEOUT_MS = 120_000;
const MAX_TIMEOUT_MS = 3_600_000;
const MAX_NAME = 100;
const MAX_DESCRIPTION = 2000;
const MAX_SECRET = 4096;

type Editable = {
  name: string;
  description: string;
  url: string;
  auth: AuthMode;
  access: Access;
  timeoutMs: number;
  dailyLimit: number | null;
};

const invalid = (message: string) => err(new CatalogError('invalid_server', message));

function validate(f: Editable): Result<Editable, CatalogError> {
  const name = f.name.trim();
  const description = f.description.trim();
  if (name.length < 1 || name.length > MAX_NAME) {
    return invalid(`server name must be 1-${MAX_NAME} characters`);
  }
  if (description.length > MAX_DESCRIPTION) {
    return invalid(`server description must be at most ${MAX_DESCRIPTION} characters`);
  }
  let url: URL;
  try {
    url = new URL(f.url);
  } catch {
    return invalid('upstream URL must be a valid http(s) URL');
  }
  if (url.protocol !== 'https:' && url.protocol !== 'http:') {
    return invalid('upstream URL must be a valid http(s) URL');
  }
  if (!Number.isInteger(f.timeoutMs) || f.timeoutMs < 1 || f.timeoutMs > MAX_TIMEOUT_MS) {
    return invalid(`timeout must be 1-${MAX_TIMEOUT_MS} ms`);
  }
  if (f.dailyLimit !== null && (!Number.isInteger(f.dailyLimit) || f.dailyLimit < 1)) {
    return invalid('daily limit must be a positive integer or empty');
  }
  const auth = AuthMode.parse(f.auth);
  if (!auth.ok) return auth;
  const access = Access.parse(f.access);
  if (!access.ok) return access;
  return ok({
    name,
    description,
    url: f.url,
    auth: auth.value,
    access: access.value,
    timeoutMs: f.timeoutMs,
    dailyLimit: f.dailyLimit,
  });
}

export interface ServerState extends Editable {
  slug: string;
  /**
   * The stored row's version when it was read (optimistic concurrency): the repository writes an
   * edit only if the row is still at it. 0 for a new server.
   */
  version: number;
  sealedSecret: Uint8Array | null;
  status: ServerStatus;
  lastSyncAt: Date | null;
  lastSyncError: string | null;
  syncUserId: string | null;
  createdAt: Date;
  updatedAt: Date;
}

/** Aggregate: an upstream MCP server in the catalog. Holds the secret only sealed (encrypted). */
export class Server {
  private events: DomainEvent[] = [];
  /**
   * Whether the secret was set, cleared or re-sealed since the server was loaded. The repository
   * writes the secret only then: a stale copy must not put back a secret someone else changed
   * meanwhile (another admin, a key rotation).
   */
  private secretDirty: boolean;

  private constructor(
    private s: ServerState,
    loaded: boolean,
  ) {
    this.secretDirty = !loaded;
  }

  static create(p: {
    slug: string;
    name: string;
    description: string;
    url: string;
    auth: AuthMode;
    access: Access;
    timeoutMs?: number;
    dailyLimit?: number | null;
    now: Date;
  }): Result<Server, CatalogError> {
    const slug = ServerSlug.parse(p.slug);
    if (!slug.ok) return slug;
    const v = validate({
      name: p.name,
      description: p.description,
      url: p.url,
      auth: p.auth,
      access: p.access,
      timeoutMs: p.timeoutMs ?? DEFAULT_TIMEOUT_MS,
      dailyLimit: p.dailyLimit ?? null,
    });
    if (!v.ok) return v;
    const server = new Server(
      {
        ...v.value,
        slug: slug.value,
        version: 0,
        sealedSecret: null,
        status: 'active',
        lastSyncAt: null,
        lastSyncError: null,
        syncUserId: null,
        createdAt: p.now,
        updatedAt: p.now,
      },
      false,
    );
    server.markChanged(p.now);
    return ok(server);
  }

  /** A secret to be stored: non-blank, at most 4096 characters. Returned as given (no trimming). */
  static checkSecret(plain: string): Result<string, CatalogError> {
    if (plain.trim().length === 0 || plain.length > MAX_SECRET) {
      return invalid(`secret must be non-blank and at most ${MAX_SECRET} characters`);
    }
    return ok(plain);
  }

  static rehydrate(state: ServerState): Server {
    return new Server({ ...state }, true);
  }

  get slug(): string {
    return this.s.slug;
  }
  get version(): number {
    return this.s.version;
  }
  get name(): string {
    return this.s.name;
  }
  get description(): string {
    return this.s.description;
  }
  get url(): string {
    return this.s.url;
  }
  get auth(): AuthMode {
    return this.s.auth;
  }
  get access(): Access {
    return this.s.access;
  }
  get timeoutMs(): number {
    return this.s.timeoutMs;
  }
  get dailyLimit(): number | null {
    return this.s.dailyLimit;
  }
  get status(): ServerStatus {
    return this.s.status;
  }
  get lastSyncAt(): Date | null {
    return this.s.lastSyncAt;
  }
  get lastSyncError(): string | null {
    return this.s.lastSyncError;
  }
  get syncUserId(): string | null {
    return this.s.syncUserId;
  }
  get createdAt(): Date {
    return this.s.createdAt;
  }
  get updatedAt(): Date {
    return this.s.updatedAt;
  }
  get secretSet(): boolean {
    return this.s.sealedSecret !== null;
  }
  /** The secret was set, cleared or re-sealed since loading (a new server: always). */
  get secretChanged(): boolean {
    return this.secretDirty;
  }
  /** The encrypted secret, for the repository and for opening it in a use case. Never serialise it. */
  get sealedSecret(): Uint8Array | null {
    return this.s.sealedSecret;
  }

  /**
   * All-or-nothing edit of the given fields. A change of name, description or access tells the
   * search index. A change of auth kind drops the stored secret (it belonged to the old mode);
   * `secretCleared` says so, for the audit.
   */
  update(changes: Partial<Editable>, now: Date): Result<{ secretCleared: boolean }, CatalogError> {
    const v = validate({ ...this.s, ...changes });
    if (!v.ok) return v;
    const searchable =
      v.value.name !== this.s.name ||
      v.value.description !== this.s.description ||
      JSON.stringify(v.value.access) !== JSON.stringify(this.s.access);
    const secretCleared = v.value.auth.kind !== this.s.auth.kind && this.s.sealedSecret !== null;
    this.s = {
      ...this.s,
      ...v.value,
      sealedSecret: secretCleared ? null : this.s.sealedSecret,
      updatedAt: now,
    };
    if (secretCleared) this.secretDirty = true;
    if (searchable) this.markChanged(now);
    return ok({ secretCleared });
  }

  /**
   * The same secret sealed under another key (SECRETS_KEY rotation). Not an edit: `updatedAt`
   * and the events stay as they are.
   */
  replaceSealedSecret(sealed: Uint8Array): void {
    this.s = { ...this.s, sealedSecret: sealed };
    this.secretDirty = true;
  }

  /** `null` clears. A secret is accepted only for a mode that holds one. */
  setSealedSecret(sealed: Uint8Array | null, now: Date): Result<void, CatalogError> {
    if (sealed !== null && !AuthMode.holdsSecret(this.s.auth)) {
      return invalid(`auth mode "${this.s.auth.kind}" does not use a server secret`);
    }
    this.s = { ...this.s, sealedSecret: sealed, updatedAt: now };
    this.secretDirty = true;
    return ok(undefined);
  }

  /** false when nothing changed. */
  setStatus(status: ServerStatus, now: Date): boolean {
    if (this.s.status === status) return false;
    this.s = { ...this.s, status, updatedAt: now };
    this.markChanged(now);
    return true;
  }

  /**
   * Whose personal connection a synchronisation uses: the admin who started it, or, for the
   * scheduler (`null`), the last admin who synchronised the server successfully.
   */
  syncCredentialsOwner(startedBy: string | null): string | null {
    return startedBy ?? this.s.syncUserId;
  }

  /**
   * Records a synchronisation attempt. `lastSyncAt` is when the latest attempt started; an attempt
   * that started before it is ignored (false), so a slow old run cannot undo a newer one. A
   * success clears the error; only a run that names `syncedBy` changes `syncUserId`.
   */
  applySync(record: SyncRecord): boolean {
    const last = this.s.lastSyncAt;
    if (last !== null && record.startedAt.getTime() < last.getTime()) return false;
    this.s = {
      ...this.s,
      lastSyncAt: record.startedAt,
      lastSyncError: record.error,
      syncUserId: record.syncedBy ?? this.s.syncUserId,
    };
    return true;
  }

  /** One pending server_changed is enough however many fields changed before the save. */
  private markChanged(now: Date): void {
    if (this.events.some((e) => e.type === SERVER_CHANGED)) return;
    this.events.push(serverChanged(this.s.slug, now));
  }

  pullEvents(): DomainEvent[] {
    const out = this.events;
    this.events = [];
    return out;
  }
}
