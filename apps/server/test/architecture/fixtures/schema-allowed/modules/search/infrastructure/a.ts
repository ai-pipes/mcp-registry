// Fixture: what the schema-ownership check lets through.
declare const sql: ((s: TemplateStringsArray, ...v: unknown[]) => Promise<unknown>) & {
  unsafe(q: string): unknown;
};
declare const ref: string;

// Its own schema and the shared platform schema.
export const own = () => sql`select ref from search.documents where ref = ${ref}`;
export const outbox = () => sql`select id from platform.outbox where type = ${'catalog.tools_changed'}`;
// Event names and other non-SQL text that merely look like schema-qualified names.
export const EVENT = 'catalog.tools_changed';
export const message = `reindexed after gateway.calls rotation`;
export const unsafe = () => sql.unsafe('select 1 from search.documents');
