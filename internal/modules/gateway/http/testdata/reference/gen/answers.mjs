// Writes ../answers.json: what the reference's handler (createMcpHandler of
// @modelcontextprotocol/server 2.4.0, stateless, with the legacy fallback, as
// apps/server/src/platform/mcp/server.ts builds it) answers to each request of ../cases.json, with
// the four meta-tools registered under their real names, descriptions and schemas and handlers
// that answer what the Go test's fakes answer (no servers, no hits, "called"). See README.md.
import { readFileSync, writeFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { fileURLToPath, pathToFileURL } from 'node:url';
const here = (name) => fileURLToPath(new URL(name, import.meta.url));
// The packages of apps/server (pnpm install), or of the checkout named by MCP_REFERENCE_ROOT.
const root = process.env.MCP_REFERENCE_ROOT ?? here('../../../../../../../../');
const require = createRequire(root.replace(/\/?$/, '/') + 'apps/server/package.json');
const { createMcpHandler, McpServer } = await import(
  pathToFileURL(require.resolve('@modelcontextprotocol/server')).href
);
const z = await import(pathToFileURL(require.resolve('zod')).href);
const INSTR =
  "This server is a gateway to the user's services (git hosting, issue tracker and others). When a task needs one of them, call search_tools with a short description of the action, then describe_tool for the chosen tool, then call_tool. If nothing fits, continue without them. If a tool needs a connection, tell the user the link from the error.";
const text = (t) => ({ content: [{ type: 'text', text: t }] });
const structured = (v) => ({ ...text(JSON.stringify(v)), structuredContent: v });
const failure = (t) => ({ ...text(t), isError: true });
const LIST_D =
  "List the user's services available through this gateway: slug, name, description, number of tools and connection state (connectUrl when the user has to connect first).";
const SEARCH_D =
  'Find tools by a short description of the action. Returns only tools above a relevance threshold, best first, with score 0–1; an empty list means nothing fits. servers (slugs from list_servers) limits the search.';
const DESCRIBE_D =
  'Full description and input JSON Schema of a tool from search_tools ("<server>.<tool>"). Call before call_tool.';
const CALL_D =
  'Call a tool ("<server>.<tool>") with arguments matching its inputSchema, on the user\'s behalf. Returns the service\'s result as is.';
const Connection = z.enum(['ready', 'needs_connection', 'expired', 'not_required']);
const Annotations = z.record(z.string(), z.boolean());
const ToolName = z.string().min(1).max(300);
function build() {
  const s = new McpServer(
    { name: 'mcp-registry', version: '1.0.1' },
    { instructions: INSTR, capabilities: { tools: { listChanged: false } } },
  );
  s.registerTool(
    'list_servers',
    {
      description: LIST_D,
      inputSchema: z.object({}),
      outputSchema: z.object({
        servers: z.array(
          z.object({
            slug: z.string(),
            name: z.string(),
            description: z.string(),
            tools: z.number(),
            connection: Connection,
            connectUrl: z.string().optional(),
          }),
        ),
      }),
      annotations: { readOnlyHint: true },
    },
    async () => structured({ servers: [] }),
  );
  s.registerTool(
    'search_tools',
    {
      description: SEARCH_D,
      inputSchema: z.object({
        query: z.string().min(1).max(500),
        limit: z.number().int().min(1).max(25).default(10),
        servers: z.array(z.string().min(1).max(64)).min(1).max(20).optional(),
      }),
      outputSchema: z.object({
        results: z.array(
          z.object({
            tool: z.string(),
            title: z.string().nullable(),
            description: z.string(),
            server: z.string(),
            score: z.number(),
            annotations: Annotations,
            connection: Connection,
          }),
        ),
      }),
      annotations: { readOnlyHint: true },
    },
    async () => structured({ results: [] }),
  );
  s.registerTool(
    'describe_tool',
    {
      description: DESCRIBE_D,
      inputSchema: z.object({ tool: ToolName }),
      outputSchema: z.object({
        tool: z.string(),
        title: z.string().nullable(),
        description: z.string(),
        inputSchema: z.record(z.string(), z.unknown()),
        outputSchema: z.record(z.string(), z.unknown()).optional(),
        annotations: Annotations,
        server: z.string(),
        connection: Connection,
      }),
      annotations: { readOnlyHint: true },
    },
    async (a) => failure(`Tool "${a.tool}" is not available. Use search_tools to find another.`),
  );
  s.registerTool(
    'call_tool',
    {
      description: CALL_D,
      inputSchema: z.object({
        tool: ToolName,
        arguments: z.record(z.string(), z.unknown()).default({}),
      }),
      annotations: { readOnlyHint: false, openWorldHint: true },
    },
    async () => text('called'),
  );
  return s;
}
const handler = createMcpHandler(() => build(), { maxRequestBodySize: 10 * 1024 * 1024 });
const cases = JSON.parse(readFileSync(here('../cases.json'), 'utf8'));
// Each request is its own exchange of a stateless handler; Promise.all keeps the cases' order.
const answer = async (c) => {
  const init = { method: c.method, headers: c.headers };
  if (c.method === 'POST') init.body = c.body;
  const res = await handler.fetch(new Request('http://localhost:3000/mcp', init), {
    authInfo: { token: 'x', clientId: 'u', scopes: [], extra: {} },
  });
  return {
    name: c.name,
    status: res.status,
    contentType: res.headers.get('content-type') ?? '',
    allow: res.headers.get('allow') ?? '',
    body: await res.text(),
  };
};
const out = await Promise.all(cases.map(answer));
writeFileSync(here('../answers.json'), JSON.stringify(out, null, 2) + '\n');
await handler.close();
