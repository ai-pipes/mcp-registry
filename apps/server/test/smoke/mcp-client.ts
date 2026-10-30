// The MCP part of scripts/smoke.sh, as Claude Code would use the gateway: connect with a Bearer
// token, then search_tools → describe_tool → call_tool. Checks that the call reached the
// upstream tool and that no answer the client got contains the upstream secret.
//
// Usage: SMOKE_TOKEN=sk_… SMOKE_UPSTREAM_SECRET=… \
//   node --experimental-strip-types apps/server/test/smoke/mcp-client.ts <base-url> <tool-ref>
// Exit codes: 0 as expected, 1 not as expected (yet: the index may still be building), 2 usage.
import { Client, StreamableHTTPClientTransport } from '@modelcontextprotocol/client';

const [baseUrl, ref] = process.argv.slice(2);
const token = process.env.SMOKE_TOKEN;
const secret = process.env.SMOKE_UPSTREAM_SECRET;
if (!baseUrl || !ref || !token || !secret) {
  process.stderr.write('Usage: SMOKE_TOKEN=… SMOKE_UPSTREAM_SECRET=… mcp-client.ts <url> <ref>\n');
  process.exit(2);
}

const seen: unknown[] = [];
const fail = (message: string): never => {
  process.stderr.write(`${message}\n`);
  process.exit(1);
};

const transport = new StreamableHTTPClientTransport(new URL('/mcp', baseUrl), {
  requestInit: { headers: { Authorization: `Bearer ${token}` } },
});
const client = new Client({ name: 'smoke', version: '1.0.0' });
await client.connect(transport);
try {
  const listed = await client.listTools();
  seen.push(listed);
  const names = listed.tools.map((t) => t.name).toSorted();
  process.stdout.write(`tools/list → ${names.join(', ')}\n`);
  if (names.join(',') !== 'call_tool,describe_tool,list_servers,search_tools') {
    fail('the gateway must expose exactly the 4 meta-tools');
  }

  const search = await client.callTool({
    name: 'search_tools',
    arguments: { query: 'echo text back' },
  });
  seen.push(search);
  const sc: unknown = search.structuredContent;
  const raw: unknown[] =
    sc !== null && typeof sc === 'object' && 'results' in sc && Array.isArray(sc.results)
      ? sc.results
      : [];
  const results = raw.flatMap((r) =>
    r !== null && typeof r === 'object' && 'tool' in r && 'score' in r
      ? [{ tool: String(r.tool), score: Number(r.score) }]
      : [],
  );
  process.stdout.write(
    `search_tools → ${results.map((r) => `${r.tool} (${r.score.toFixed(3)})`).join(', ') || 'no results'}\n`,
  );
  if (search.isError || results[0]?.tool !== ref) fail(`search_tools did not find ${ref} first`);

  const described = await client.callTool({ name: 'describe_tool', arguments: { tool: ref } });
  seen.push(described);
  if (described.isError) fail(`describe_tool(${ref}) failed: ${JSON.stringify(described.content)}`);
  process.stdout.write(`describe_tool(${ref}) → ok\n`);

  const called = await client.callTool({
    name: 'call_tool',
    arguments: { tool: ref, arguments: { text: 'hello from smoke' } },
  });
  seen.push(called);
  const text = JSON.stringify(called.content);
  if (called.isError || !text.includes('hello from smoke')) {
    fail(`call_tool(${ref}) did not echo: ${text}`);
  }
  process.stdout.write(`call_tool(${ref}) → ${text}\n`);

  if (JSON.stringify(seen).includes(secret)) fail('the client saw the upstream secret');
  process.stdout.write('client never saw the upstream secret\n');
} finally {
  await client.close();
}
