import type { Result } from '../../../shared-kernel/index.ts';
import type { Access, ServerStatus } from '../domain/access.ts';
import type { AuthMode } from '../domain/auth-mode.ts';
import type { Server } from '../domain/server.ts';
import type { PersonalCredentialFailure, SyncRecord } from '../domain/sync.ts';
import type { Tool, ToolAnnotations, ToolSnapshot } from '../domain/tool.ts';

/**
 * Who is acting. Structurally the same as the identity module's Principal minus fields the catalog
 * does not need, so identity's value can be passed as is.
 */
export type Principal = { userId: string; role: 'admin' | 'member'; groups: string[] };

export type ServerView = {
  slug: string;
  name: string;
  description: string;
  url: string;
  auth: AuthMode;
  secretSet: boolean;
  access: Access;
  timeoutMs: number;
  dailyLimit: number | null;
  status: ServerStatus;
  lastSyncAt: Date | null;
  lastSyncError: string | null;
  toolCount: number;
};

/** What a member sees of a server they may use: no URL, no auth config, no sync state. */
export type MemberServerView = {
  slug: string;
  name: string;
  description: string;
  toolCount: number;
  authKind: AuthMode['kind'];
};

export type ToolView = {
  server: string;
  name: string;
  ref: string;
  title: string | null;
  description: string;
  inputSchema: Record<string, unknown>;
  outputSchema: Record<string, unknown> | null;
  annotations: ToolAnnotations;
  hidden: boolean;
};

/** What the gateway needs to call an upstream. No secret: it is read separately, on demand. */
export type ServerCallTarget = {
  slug: string;
  /** Display name, for messages to the agent. */
  name: string;
  url: string;
  auth: AuthMode;
  timeoutMs: number;
  dailyLimit: number | null;
};

/** A server with the number of its tools that are visible (not hidden, not removed). */
export type ServerListing = { server: Server; toolCount: number };

export interface ServerRepository {
  findBySlug(slug: string): Promise<Server | null>;
  /** Ordered by slug; tool counts come from the same query. */
  list(): Promise<ServerListing[]>;
  /** Inserts and drains the aggregate's events into the outbox. A taken slug throws CatalogError server_exists. */
  add(server: Server): Promise<void>;
  /** Updates an existing server and drains its events into the outbox. Does not touch synchronisation state. */
  save(server: Server): Promise<void>;
  /**
   * Writes only the synchronisation state, by the rule of `Server.applySync`, atomically: an
   * attempt older than the stored `last_sync_at` changes nothing, and `sync_user_id` changes only
   * when `record.syncedBy` is set. Throws CatalogError not_found for an unknown server.
   */
  saveSyncState(slug: string, record: SyncRecord): Promise<void>;
  /**
   * Swaps the sealed secret only while it still holds `current` (compare-and-set), touching
   * nothing else: no `updated_at`, no events. False when it changed meanwhile or the server is gone.
   */
  replaceSealedSecret(slug: string, current: Uint8Array, next: Uint8Array): Promise<boolean>;
}

export interface ToolRepository {
  find(server: string, name: string): Promise<Tool | null>;
  /** Every stored tool of the server, removed ones included, ordered by name. */
  listByServer(server: string): Promise<Tool[]>;
  /** Persists the `hidden` flag and drains the tool's events into the outbox. */
  save(tool: Tool): Promise<void>;
  /**
   * In one transaction: reconciles the stored tools of the server with the snapshot (domain
   * `reconcileTools`), writes the result and appends its events to the outbox.
   */
  applySnapshot(
    server: string,
    snapshot: readonly ToolSnapshot[],
    now: Date,
  ): Promise<SnapshotResult>;
}

/** `addedRefs` ⊆ `changedRefs`: tools that were absent or removed before. */
export type SnapshotResult = { changedRefs: string[]; addedRefs: string[]; removedRefs: string[] };

/**
 * What a synchronisation of an `oauth_user`/`pat_user` server needs from other modules: whether
 * its credentials owner is still an enabled admin (identity) and that person's own upstream
 * secret, raw — the OAuth access token or the PAT (connections). Platform implements it over
 * their public APIs, without rules of its own: which owner may lend a connection and what a
 * credential failure means for the synchronisation are the catalog's (`SyncOwner`). The catalog
 * builds the header itself (`AuthMode.headerFor`).
 */
export interface SyncCredentials {
  isActiveAdmin(userId: string): Promise<boolean>;
  personalSecret(
    server: ServerCallTarget,
    userId: string,
  ): Promise<Result<string, PersonalCredentialFailure>>;
}
