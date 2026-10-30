import type { DomainEvent } from '../../../shared-kernel/index.ts';

export const toolsChanged = (server: string, refs: string[], occurredAt: Date): DomainEvent => ({
  type: 'catalog.tools_changed',
  occurredAt,
  payload: { server, refs },
});

export const toolsRemoved = (server: string, refs: string[], occurredAt: Date): DomainEvent => ({
  type: 'catalog.tools_removed',
  occurredAt,
  payload: { server, refs },
});

export const SERVER_CHANGED = 'catalog.server_changed';

/** Name, description or status of a server changed: the search index must follow. */
export const serverChanged = (server: string, occurredAt: Date): DomainEvent => ({
  type: SERVER_CHANGED,
  occurredAt,
  payload: { server },
});
