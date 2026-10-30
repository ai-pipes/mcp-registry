import {
  ConnectCallbackError,
  ConnectCallbackQuery,
  type ConnectionSummary,
  SavePatRequest,
  type StartConnectResponse,
} from '@mcp/contracts';
import { type Context, Hono } from 'hono';
import { readJson } from '../../../../platform/http/request.ts';
import {
  type IdentityDeps,
  type IdentityEnv,
  requireSession,
  sessionPrincipal,
} from '../../../identity/index.ts';
import { completeConnect, disconnect, savePat, startConnect } from '../../application/connect.ts';
import { listConnections } from '../../application/list.ts';
import type { ConnectionsDeps } from '../../application/ports.ts';

const toPage = (c: Context, query: string) => c.redirect(`/connections?${query}`, 302);
const failed = (c: Context, code: ConnectCallbackError) =>
  toPage(c, `error=${encodeURIComponent(code)}`);

/** `/api/connections`: the person's own credentials to upstream servers. */
export function connectionRoutes(
  deps: ConnectionsDeps,
  identity: IdentityDeps,
  opts: { logError: (error: unknown, message: string) => void },
) {
  return (
    new Hono<IdentityEnv>()
      // The provider sends the browser back here: every outcome is a redirect to the UI page,
      // so this route resolves the session itself instead of answering 401.
      .get('/callback', async (c) => {
        const principal = await sessionPrincipal(identity, c);
        if (!principal) return failed(c, 'unauthenticated');
        const parsed = ConnectCallbackQuery.safeParse(c.req.query());
        // No code: the person declined at the provider, or the answer is broken.
        if (!parsed.success) return failed(c, 'denied');
        let r;
        try {
          r = await completeConnect(deps, { principal, ...parsed.data });
        } catch (error) {
          // Back from the provider: a page with a fixed code, not a JSON 500.
          opts.logError(error, 'connections: connect callback failed');
          return failed(c, 'internal');
        }
        if (!r.ok) {
          const known = ConnectCallbackError.safeParse(r.error.code);
          return failed(c, known.success ? known.data : 'exchange_failed');
        }
        return toPage(c, `connected=${encodeURIComponent(r.value.server)}`);
      })
      .use(requireSession(identity))
      .get('/', async (c) => {
        const list: ConnectionSummary[] = await listConnections(deps, {
          principal: c.get('principal'),
        });
        return c.json(list);
      })
      .post('/:server/start', async (c) => {
        const r = await startConnect(deps, {
          principal: c.get('principal'),
          server: c.req.param('server'),
        });
        if (!r.ok) throw r.error;
        const body: StartConnectResponse = { url: r.value.toString() };
        return c.json(body);
      })
      .put('/:server/pat', async (c) => {
        const { token } = await readJson(c, SavePatRequest);
        const r = await savePat(deps, {
          principal: c.get('principal'),
          server: c.req.param('server'),
          token,
        });
        if (!r.ok) throw r.error;
        return c.body(null, 204);
      })
      .delete('/:server', async (c) => {
        await disconnect(deps, { principal: c.get('principal'), server: c.req.param('server') });
        return c.body(null, 204);
      })
  );
}
