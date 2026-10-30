// Fixture: a module joining another module's table in SQL.
declare const sql: (s: TemplateStringsArray, ...v: unknown[]) => Promise<unknown>;
declare const userId: string;

export const rows = () => sql`select c.server, s.name from connections.connections c
  join catalog.servers s on s.slug = c.server where c.user_id = ${userId}`;
