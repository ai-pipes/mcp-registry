import { indexTools, removeTools } from './index-tools.ts';
import type { SearchDeps } from './ports.ts';

export type EventHandler = (payload: Record<string, unknown>) => Promise<void>;

function serverOf(payload: Record<string, unknown>): string {
  const server = payload['server'];
  if (typeof server !== 'string') throw new Error('event payload has no server');
  return server;
}

function refsOf(payload: Record<string, unknown>): string[] {
  const refs = payload['refs'];
  if (!Array.isArray(refs) || !refs.every((r): r is string => typeof r === 'string')) {
    throw new Error('event payload has no refs');
  }
  return refs;
}

/** Outbox handlers for the catalog events that change what is searchable. */
export function searchEventHandlers(deps: SearchDeps): Record<string, EventHandler> {
  return {
    'catalog.tools_changed': async (p) =>
      indexTools(deps, { server: serverOf(p), refs: refsOf(p) }),
    'catalog.tools_removed': async (p) => removeTools(deps, { refs: refsOf(p) }),
    // Name, description or status of the server: every passage carries them.
    'catalog.server_changed': async (p) => indexTools(deps, { server: serverOf(p) }),
  };
}
