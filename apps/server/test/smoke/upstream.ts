// Upstream MCP server for scripts/smoke.sh, run in its own container (Dockerfile target
// `smoke-upstream`, never part of the production image) with
//   node --experimental-strip-types apps/server/test/smoke/upstream.ts
// Serves one `echo` tool on 0.0.0.0:8080/mcp and answers 401 to any request without
// `X-Smoke-Key: Key <SMOKE_UPSTREAM_SECRET>`. Logs one line per request — the JSON-RPC method and
// whether the key matched, never the key itself — which smoke.sh checks.
import { startFakeUpstream } from '../fake-upstream.ts';

const secret = process.env.SMOKE_UPSTREAM_SECRET;
if (!secret) {
  process.stderr.write('SMOKE_UPSTREAM_SECRET is not set\n');
  process.exit(2);
}
const HEADER = 'x-smoke-key';
const expected = `Key ${secret}`;

const up = await startFakeUpstream(
  [
    {
      name: 'echo',
      title: 'Echo',
      description: 'Echo the given text back unchanged. Use it to check that calls go through.',
      inputSchema: { properties: { text: { type: 'string' } }, required: ['text'] },
    },
  ],
  { hostname: '0.0.0.0', port: 8080, requireHeader: { name: HEADER, value: expected } },
);

let logged = 0;
setInterval(() => {
  for (const r of up.requests.slice(logged)) {
    const authorized = r.headers[HEADER] === expected;
    process.stdout.write(`request ${r.method ?? '-'} authorized=${authorized}\n`);
  }
  logged = up.requests.length;
}, 200);
process.stdout.write('smoke upstream listening on :8080\n');
for (const signal of ['SIGTERM', 'SIGINT'] as const) {
  process.on(signal, () => {
    void up.close().finally(() => process.exit(0));
  });
}
