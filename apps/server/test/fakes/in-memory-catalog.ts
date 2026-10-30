import type { DomainEvent } from '../../src/shared-kernel/index.ts';
import type { CatalogDeps } from '../../src/modules/catalog/application/deps.ts';
import type {
  ServerListing,
  ServerRepository,
  ToolRepository,
} from '../../src/modules/catalog/application/ports.ts';
import { CatalogError } from '../../src/modules/catalog/domain/errors.ts';
import type { Server } from '../../src/modules/catalog/domain/server.ts';
import type { SyncRecord } from '../../src/modules/catalog/domain/sync.ts';
import {
  reconcileTools,
  type Tool,
  type ToolSnapshot,
} from '../../src/modules/catalog/domain/tool.ts';

type Audited = {
  actorId: string | null;
  action: string;
  target?: string;
  meta?: Record<string, unknown>;
};

class InMemoryServerRepository implements ServerRepository {
  readonly rows = new Map<string, Server>();
  constructor(
    private readonly outbox: DomainEvent[],
    private readonly tools: InMemoryToolRepository,
  ) {}

  findBySlug(slug: string): Promise<Server | null> {
    return Promise.resolve(this.rows.get(slug) ?? null);
  }

  list(): Promise<ServerListing[]> {
    const out = [...this.rows.values()]
      .toSorted((a, b) => (a.slug < b.slug ? -1 : 1))
      .map((server) => ({ server, toolCount: this.tools.visibleCount(server.slug) }));
    return Promise.resolve(out);
  }

  add(server: Server): Promise<void> {
    if (this.rows.has(server.slug)) {
      return Promise.reject(new CatalogError('server_exists', `server "${server.slug}" exists`));
    }
    this.rows.set(server.slug, server);
    this.outbox.push(...server.pullEvents());
    return Promise.resolve();
  }

  save(server: Server): Promise<void> {
    this.rows.set(server.slug, server);
    this.outbox.push(...server.pullEvents());
    return Promise.resolve();
  }

  saveSyncState(slug: string, record: SyncRecord): Promise<void> {
    const server = this.rows.get(slug);
    if (!server) return Promise.reject(new CatalogError('not_found', `server "${slug}" not found`));
    server.applySync(record);
    return Promise.resolve();
  }

  replaceSealedSecret(slug: string, current: Uint8Array, next: Uint8Array): Promise<boolean> {
    const server = this.rows.get(slug);
    const stored = server?.sealedSecret;
    if (!server || !stored || Buffer.compare(Buffer.from(stored), Buffer.from(current)) !== 0) {
      return Promise.resolve(false);
    }
    server.replaceSealedSecret(next);
    return Promise.resolve(true);
  }
}

class InMemoryToolRepository implements ToolRepository {
  readonly rows = new Map<string, Tool>();
  constructor(private readonly outbox: DomainEvent[]) {}

  private key = (server: string, name: string) => `${server}\u0000${name}`;

  visibleCount(server: string): number {
    return [...this.rows.values()].filter((t) => t.server === server && t.visible).length;
  }

  put(tool: Tool): void {
    this.rows.set(this.key(tool.server, tool.name), tool);
  }

  find(server: string, name: string): Promise<Tool | null> {
    return Promise.resolve(this.rows.get(this.key(server, name)) ?? null);
  }

  listByServer(server: string): Promise<Tool[]> {
    return Promise.resolve(
      [...this.rows.values()]
        .filter((t) => t.server === server)
        .toSorted((a, b) => (a.name < b.name ? -1 : 1)),
    );
  }

  save(tool: Tool): Promise<void> {
    this.put(tool);
    this.outbox.push(...tool.pullEvents());
    return Promise.resolve();
  }

  async applySnapshot(server: string, snapshot: readonly ToolSnapshot[], now: Date) {
    const r = reconcileTools(server, await this.listByServer(server), snapshot, now);
    for (const t of r.upserts) this.put(t);
    this.outbox.push(...r.events);
    return { changedRefs: r.changedRefs, addedRefs: r.addedRefs, removedRefs: r.removedRefs };
  }
}

/** Reversible stand-in for AES: visible enough in assertions that nothing leaks the plaintext. */
const fakeSecretBox = {
  seal: (plain: string) =>
    new TextEncoder().encode(`sealed:${Buffer.from(plain).toString('base64')}`),
  open: (sealed: Uint8Array) =>
    Buffer.from(new TextDecoder().decode(sealed).slice('sealed:'.length), 'base64').toString(),
};

export function makeCatalogDeps(now = new Date('2026-10-30T10:00:00Z')) {
  const outbox: DomainEvent[] = [];
  const audit: Audited[] = [];
  const clock = { current: now, now: () => clock.current };
  const tools = new InMemoryToolRepository(outbox);
  const servers = new InMemoryServerRepository(outbox, tools);
  const deps: CatalogDeps = {
    servers,
    tools,
    secretBox: fakeSecretBox,
    clock,
    audit: {
      record: (e) => {
        audit.push(e);
        return Promise.resolve();
      },
    },
  };
  return { deps, servers, tools, outbox, audit, clock };
}
