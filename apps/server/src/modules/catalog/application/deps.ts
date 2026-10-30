import type { AuditLog, Clock, SecretBox, UpstreamClient } from '../../../shared-kernel/index.ts';
import type { ServerRepository, SyncCredentials, ToolRepository } from './ports.ts';

export type CatalogDeps = {
  servers: ServerRepository;
  tools: ToolRepository;
  secretBox: SecretBox;
  clock: Clock;
  audit: AuditLog;
};

/** Catalog synchronisation also talks to upstream. */
export type SyncDeps = CatalogDeps & {
  upstream: UpstreamClient;
  credentials: SyncCredentials;
  /** Upstream answers above this size fail the synchronisation (UPSTREAM_MAX_RESPONSE_BYTES). */
  maxResponseBytes: number;
  /** Receives unexpected errors that are absorbed rather than propagated. Fields carry no secrets. */
  logger?: { error(fields: Record<string, unknown>, message: string): void };
};
