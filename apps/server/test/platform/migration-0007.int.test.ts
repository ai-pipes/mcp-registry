import { afterAll, beforeAll, describe, expect, it } from 'vite-plus/test';
import { serverFingerprint } from '../../src/modules/connections/domain/server-fingerprint.ts';
import { migrate } from '../../src/platform/migrate.ts';
import { migrationsDir, startPg } from '../pg.ts';

let pg: Awaited<ReturnType<typeof startPg>>;
beforeAll(async () => {
  pg = await startPg();
});
afterAll(async () => {
  await pg.stop();
});

const OAUTH = { authorizeUrl: 'https://p/a', tokenUrl: 'https://p/t', clientId: 'cid' };
const UPSTREAM = 'https://mcp.example.com/gitea';
/** `issued_for` as written before 0007: the revoke URL was the sixth element. */
const oldFingerprint = (revokeUrl: string | null) =>
  JSON.stringify([
    UPSTREAM,
    'oauth_user',
    OAUTH.authorizeUrl,
    OAUTH.tokenUrl,
    OAUTH.clientId,
    revokeUrl,
  ]);

describe('0007_connection_revoke_url', () => {
  it('moves the revoke URL out of issued_for: existing connections stay bound, revoke kept', async () => {
    // Back to the state before 0007, with connections written by the old code.
    await pg.sql`alter table connections.connections drop column revoke_url`;
    await pg.sql`delete from platform.schema_migrations where name = '0007_connection_revoke_url.sql'`;
    const insert = (userId: string, issuedFor: string, kind = 'oauth') => pg.sql`
      insert into connections.connections
        (user_id, server, kind, access, refresh, expires_at, status, issued_for, updated_at)
      values (${userId}, 'gitea', ${kind}, '\\x00', null, null, 'ready', ${issuedFor}, now())`;
    await insert('00000000-0000-0000-0000-000000000001', oldFingerprint('https://p/r?x="1"'));
    await insert('00000000-0000-0000-0000-000000000002', oldFingerprint(null));
    await insert(
      '00000000-0000-0000-0000-000000000003',
      JSON.stringify([UPSTREAM, 'pat_user', null, null, null, null]),
      'pat',
    );
    await pg.sql`insert into connections.pending
        (state, user_id, server, code_verifier, issued_for, expires_at)
      values ('s1', '00000000-0000-0000-0000-000000000001', 'gitea', 'v',
        ${oldFingerprint('https://p/r')}, now() + interval '10 minutes')`;

    expect(await migrate(pg.sql, migrationsDir)).toEqual(['0007_connection_revoke_url.sql']);

    const rows = await pg.sql<{ user_id: string; issued_for: string; revoke_url: string | null }[]>`
      select user_id, issued_for, revoke_url from connections.connections order by user_id`;
    const oauthNow = serverFingerprint({ url: UPSTREAM, authKind: 'oauth_user', oauth: OAUTH });
    const patNow = serverFingerprint({ url: UPSTREAM, authKind: 'pat_user', oauth: null });
    expect(rows.map((r) => [r.issued_for, r.revoke_url])).toEqual([
      [oauthNow, 'https://p/r?x="1"'],
      [oauthNow, null],
      [patNow, null],
    ]);
    const [pending] = await pg.sql<{ issued_for: string }[]>`
      select issued_for from connections.pending`;
    expect(pending?.issued_for).toBe(oauthNow);
  });
});
