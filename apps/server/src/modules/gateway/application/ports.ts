import type { ServerCallTarget, ToolView } from '../../catalog/index.ts';
import type { CredentialFailure } from '../../connections/index.ts';
import type { Clock, Result, UpstreamClient } from '../../../shared-kernel/index.ts';
import type { CallOutcome } from '../domain/call-outcome.ts';

/**
 * Who calls. Structurally identity's Principal minus what the gateway does not need, so
 * identity's value can be passed as is. `tokenId` is set when the call came with an `sk_…` token.
 */
export type Principal = {
  userId: string;
  role: 'admin' | 'member';
  groups: string[];
  tokenId?: string;
};

/** The catalog as the gateway sees it. Implemented over the catalog's public queries. */
export interface ToolCatalog {
  /** A tool this principal may call right now; null for unknown, hidden, removed, disabled or closed. */
  toolFor(principal: Principal, ref: string): Promise<ToolView | null>;
  /** Call parameters of an active server; null when unknown or disabled. */
  serverFor(slug: string): Promise<ServerCallTarget | null>;
  /** The server's own decrypted secret (`shared_secret`); null when unset. */
  serverSecret(slug: string): Promise<string | null>;
}

/** The person's own upstream secret (OAuth access token or PAT). Implemented over connections. */
export interface PersonalCredentials {
  /** `forceRefresh`: upstream rejected the token; never returns the old one then. */
  resolve(q: {
    userId: string;
    server: string;
    forceRefresh?: boolean;
  }): Promise<Result<string, CredentialFailure>>;
  /** Upstream rejected a credential that cannot be renewed: the connection expires. */
  markExpired(q: { userId: string; server: string }): Promise<void>;
}

export type ArgsCheck = { ok: true } | { ok: false; path: string; message: string };

/**
 * Checks call arguments against the tool's `inputSchema` (JSON Schema from upstream). Advisory:
 * upstream validates too. `ref` only names the tool in diagnostics.
 */
export interface ArgsValidator {
  validate(ref: string, schema: Record<string, unknown>, args: unknown): ArgsCheck;
}

/** One `call_tool`, as the call log keeps it. Never arguments or results. */
export type CallLogEntry = {
  userId: string;
  tokenId: string | null;
  server: string;
  tool: string;
  startedAt: Date;
  durationMs: number;
  outcome: CallOutcome;
};

export type CallLogFilter = {
  userId?: string;
  server?: string;
  outcome?: CallOutcome;
  /** Only entries started strictly before this moment (paging back in time). */
  before?: Date;
  limit: number;
};

export interface CallLog {
  record(e: CallLogEntry): Promise<void>;
  /** Newest first. */
  list(filter: CallLogFilter): Promise<(CallLogEntry & { id: string })[]>;
  /** Deletes entries started before `d`; returns how many. */
  deleteOlderThan(d: Date): Promise<number>;
}

export interface Quota {
  /**
   * Counts one call of `userId` to `server` on `day` (UTC, `YYYY-MM-DD`) if fewer than `limit`
   * were counted, atomically: concurrent callers never exceed the limit together.
   */
  tryConsume(input: {
    userId: string;
    server: string;
    day: string;
    limit: number;
  }): Promise<{ ok: true } | { ok: false; resetAt: Date }>;
  /** Deletes counters of days before `day` (`YYYY-MM-DD`); returns how many. */
  deleteDaysBefore(day: string): Promise<number>;
}

/** Structured logger (pino satisfies it). Fields never carry call arguments or results. */
export interface GatewayLogger {
  error(fields: Record<string, unknown>, message: string): void;
}

export type GatewayDeps = {
  catalog: ToolCatalog;
  credentials: PersonalCredentials;
  validator: ArgsValidator;
  callLog: CallLog;
  quota: Quota;
  upstream: UpstreamClient;
  clock: Clock;
  logger: GatewayLogger;
  /** PUBLIC_URL, for the link to the connections page. */
  publicUrl: string;
  /** UPSTREAM_MAX_RESPONSE_BYTES. */
  maxResponseBytes: number;
};
