import {
  type DomainEvent,
  err,
  ok,
  type Result,
  type UpstreamTool,
} from '../../../shared-kernel/index.ts';
import { CatalogError } from './errors.ts';
import { toolsChanged, toolsRemoved } from './events.ts';
import { ToolRef } from './tool-ref.ts';

export interface ToolAnnotations {
  readOnlyHint?: boolean;
  destructiveHint?: boolean;
  idempotentHint?: boolean;
  openWorldHint?: boolean;
}

/** What upstream `tools/list` says about a tool. */
export interface ToolSnapshot {
  name: string;
  title: string | null;
  description: string;
  inputSchema: Record<string, unknown>;
  outputSchema: Record<string, unknown> | null;
  annotations: ToolAnnotations;
}

const HINTS = ['readOnlyHint', 'destructiveHint', 'idempotentHint', 'openWorldHint'] as const;

/** Normalises an upstream tool: absent fields get their stored defaults, only boolean hints are kept. */
export function snapshotFromUpstream(u: UpstreamTool): ToolSnapshot {
  const annotations: ToolAnnotations = {};
  for (const hint of HINTS) {
    const value = u.annotations?.[hint];
    if (typeof value === 'boolean') annotations[hint] = value;
  }
  return {
    name: u.name,
    title: u.title ?? null,
    description: u.description ?? '',
    inputSchema: u.inputSchema,
    outputSchema: u.outputSchema ?? null,
    annotations,
  };
}

export interface ToolState extends ToolSnapshot {
  server: string;
  hidden: boolean;
  removedAt: Date | null;
  syncedAt: Date;
}

/** A tool snapshot plus the admin's `hidden` flag (which outlives synchronisation). */
export class Tool {
  private events: DomainEvent[] = [];
  private isHidden: boolean;
  readonly server: string;
  readonly name: string;
  readonly title: string | null;
  readonly description: string;
  readonly inputSchema: Record<string, unknown>;
  readonly outputSchema: Record<string, unknown> | null;
  readonly annotations: ToolAnnotations;
  readonly removedAt: Date | null;
  readonly syncedAt: Date;

  private constructor(s: ToolState) {
    this.server = s.server;
    this.name = s.name;
    this.title = s.title;
    this.description = s.description;
    this.inputSchema = s.inputSchema;
    this.outputSchema = s.outputSchema;
    this.annotations = s.annotations;
    this.isHidden = s.hidden;
    this.removedAt = s.removedAt;
    this.syncedAt = s.syncedAt;
  }

  static rehydrate(s: ToolState): Tool {
    return new Tool(s);
  }

  get ref(): string {
    return ToolRef.format(this.server, this.name);
  }

  get hidden(): boolean {
    return this.isHidden;
  }

  /** Searchable and callable: not hidden by an admin and still offered by upstream. */
  get visible(): boolean {
    return !this.isHidden && !this.isRemoved;
  }

  get isRemoved(): boolean {
    return this.removedAt !== null;
  }

  /** ok(false) when nothing changed. A tool upstream no longer offers cannot be hidden or unhidden. */
  setHidden(hidden: boolean, now: Date): Result<boolean, CatalogError> {
    if (this.isRemoved) return err(new CatalogError('not_found', `tool "${this.ref}" not found`));
    if (this.isHidden === hidden) return ok(false);
    this.isHidden = hidden;
    this.events.push(toolsChanged(this.server, [this.ref], now));
    return ok(true);
  }

  pullEvents(): DomainEvent[] {
    const out = this.events;
    this.events = [];
    return out;
  }
}

/** JSON with sorted keys, so jsonb round-trips (which reorder keys) do not look like changes. */
function canonical(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(canonical).join(',')}]`;
  if (value !== null && typeof value === 'object') {
    const entries = Object.entries(value)
      .filter(([, v]) => v !== undefined)
      .toSorted(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
    return `{${entries.map(([k, v]) => `${JSON.stringify(k)}:${canonical(v)}`).join(',')}}`;
  }
  return JSON.stringify(value) ?? 'null';
}

const sameContent = (a: ToolSnapshot, b: ToolSnapshot): boolean =>
  a.title === b.title &&
  a.description === b.description &&
  canonical(a.inputSchema) === canonical(b.inputSchema) &&
  canonical(a.outputSchema) === canonical(b.outputSchema) &&
  canonical(a.annotations) === canonical(b.annotations);

export interface Reconciliation {
  /** Tools to write: new, changed, revived or newly removed. Unchanged ones are absent. */
  upserts: Tool[];
  /** New, changed and revived tools (one tools_changed event). */
  changedRefs: string[];
  /** The part of `changedRefs` that was absent or removed before. */
  addedRefs: string[];
  removedRefs: string[];
  events: DomainEvent[];
}

/**
 * Merges an upstream `tools/list` snapshot into the stored tools of one server: new and changed
 * tools are upserted, vanished ones are marked removed, returned ones revived; `hidden` is kept.
 */
export function reconcileTools(
  server: string,
  existing: readonly Tool[],
  incoming: readonly ToolSnapshot[],
  now: Date,
): Reconciliation {
  const byName = new Map(existing.map((t) => [t.name, t]));
  const seen = new Set<string>();
  const upserts: Tool[] = [];
  const changedRefs: string[] = [];
  const addedRefs: string[] = [];
  for (const snap of incoming) {
    if (seen.has(snap.name)) continue;
    seen.add(snap.name);
    const old = byName.get(snap.name);
    if (old && old.removedAt === null && sameContent(old, snap)) continue;
    const next = Tool.rehydrate({
      ...snap,
      server,
      hidden: old?.hidden ?? false,
      removedAt: null,
      syncedAt: now,
    });
    upserts.push(next);
    changedRefs.push(next.ref);
    if (!old || old.removedAt !== null) addedRefs.push(next.ref);
  }
  const removedRefs: string[] = [];
  for (const old of existing) {
    if (seen.has(old.name) || old.removedAt !== null) continue;
    const gone = Tool.rehydrate({
      server,
      name: old.name,
      title: old.title,
      description: old.description,
      inputSchema: old.inputSchema,
      outputSchema: old.outputSchema,
      annotations: old.annotations,
      hidden: old.hidden,
      removedAt: now,
      syncedAt: old.syncedAt,
    });
    upserts.push(gone);
    removedRefs.push(gone.ref);
  }
  const events: DomainEvent[] = [];
  if (changedRefs.length > 0) events.push(toolsChanged(server, changedRefs, now));
  if (removedRefs.length > 0) events.push(toolsRemoved(server, removedRefs, now));
  return { upserts, changedRefs, addedRefs, removedRefs, events };
}
