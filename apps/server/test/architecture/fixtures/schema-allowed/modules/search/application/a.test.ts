// Fixture: tests may read other schemas to check results.
declare const sql: (s: TemplateStringsArray, ...v: unknown[]) => Promise<unknown>;

export const check = () => sql`select * from catalog.tools`;
