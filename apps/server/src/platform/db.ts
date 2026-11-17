import postgres from 'postgres';

export type Sql = postgres.Sql;
export type Tx = postgres.TransactionSql;

/** `opts.password` (from PGPASSWORD_FILE) overrides the URL and PGPASSWORD; without it the client's own rules apply. */
export function createDb(url: string, opts: { password?: string } = {}): Sql {
  return postgres(url, {
    onnotice: () => undefined,
    ...(opts.password !== undefined ? { password: opts.password } : {}),
  });
}
