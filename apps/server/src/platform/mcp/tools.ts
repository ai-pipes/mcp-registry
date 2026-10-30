import {
  type CallToolResult,
  isCallToolResult,
  type McpServer,
} from '@modelcontextprotocol/server';
import { z } from 'zod';
import { getToolFor, listServersFor, type ToolView } from '../../modules/catalog/index.ts';
import { listConnections } from '../../modules/connections/index.ts';
import { AgentConnection, CALL_TOOL_TEXT, callTool } from '../../modules/gateway/index.ts';
import type { Principal } from '../../modules/identity/index.ts';
import { searchTools } from '../../modules/search/index.ts';
import { errorFields } from '../../shared-kernel/index.ts';
import type { Container } from '../container.ts';
import { shortDescription } from './text.ts';

const LIST_SERVERS_DESCRIPTION =
  "List the user's services available through this gateway: slug, name, description, number of tools and connection state (connectUrl when the user has to connect first).";
const SEARCH_DESCRIPTION =
  'Find tools by a short description of the action. Returns only tools above a relevance threshold, best first, with score 0–1; an empty list means nothing fits. servers (slugs from list_servers) limits the search.';
const DESCRIBE_DESCRIPTION =
  'Full description and input JSON Schema of a tool from search_tools ("<server>.<tool>"). Call before call_tool.';
const CALL_DESCRIPTION =
  'Call a tool ("<server>.<tool>") with arguments matching its inputSchema, on the user\'s behalf. Returns the service\'s result as is.';

const Connection = z.enum(AgentConnection.all);
const Annotations = z.record(z.string(), z.boolean());
const ToolName = z.string().min(1).max(300);

const text = (t: string): CallToolResult => ({ content: [{ type: 'text', text: t }] });
const failure = (t: string): CallToolResult => ({ ...text(t), isError: true });
const structured = (value: Record<string, unknown>): CallToolResult => ({
  ...text(JSON.stringify(value)),
  structuredContent: value,
});

/**
 * Unexpected failures (database down, …) become a generic tool error; the SDK would otherwise
 * send the raw exception message to the client. Logged without arguments or results.
 */
function guarded<A>(
  c: Container,
  tool: string,
  fn: (args: A) => Promise<CallToolResult>,
): (args: A) => Promise<CallToolResult> {
  return async (args) => {
    try {
      return await fn(args);
    } catch (error) {
      c.logger.error({ tool, err: errorFields(error) }, 'mcp tool failed');
      return failure('Internal error.');
    }
  };
}

type ServerEntry = {
  slug: string;
  name: string;
  description: string;
  tools: number;
  connection: AgentConnection;
  connectUrl?: string;
};

/**
 * The servers this person may use right now (catalog decides), by slug, with what the agent is
 * told about the connection to each (gateway's `AgentConnection` over `listConnections`).
 */
async function usableServers(
  c: Container,
  principal: Principal,
): Promise<Map<string, ServerEntry>> {
  const [servers, connections] = await Promise.all([
    listServersFor(c.catalog, { principal }),
    listConnections(c.connections, { principal }),
  ]);
  const status = new Map(connections.map((k) => [k.server, k.status]));
  const connectUrl = `${c.gateway.publicUrl}/connections`;
  return new Map(
    servers.map((s) => {
      const connection = AgentConnection.of(s.authKind, status.get(s.slug));
      const entry: ServerEntry = {
        slug: s.slug,
        name: s.name,
        description: s.description,
        tools: s.toolCount,
        connection,
      };
      if (AgentConnection.needsLink(connection)) entry.connectUrl = connectUrl;
      return [s.slug, entry];
    }),
  );
}

const connectionOf = (state: Map<string, ServerEntry>, server: string): AgentConnection =>
  state.get(server)?.connection ?? 'needs_connection';

/** The tool if this person may use it now; catalog's single not_found covers everything else. */
async function toolFor(c: Container, principal: Principal, ref: string): Promise<ToolView | null> {
  const found = await getToolFor(c.catalog, { principal, ref });
  return found.ok ? found.value : null;
}

/**
 * The four meta-tools. Their set and schemas never depend on the catalog: adding a server adds
 * nothing to the agent's start-up context. Access, hiding, limits and credentials are decided by
 * the modules (catalog `listServersFor`/`getToolFor`, gateway `callTool`), not here.
 */
export function registerTools(server: McpServer, c: Container, principal: Principal): void {
  server.registerTool(
    'list_servers',
    {
      description: LIST_SERVERS_DESCRIPTION,
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
    guarded(c, 'list_servers', async () => {
      const state = await usableServers(c, principal);
      return structured({ servers: [...state.values()] });
    }),
  );

  server.registerTool(
    'search_tools',
    {
      description: SEARCH_DESCRIPTION,
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
    guarded(
      c,
      'search_tools',
      async (args: { query: string; limit: number; servers?: string[] | undefined }) => {
        const state = await usableServers(c, principal);
        // A server the person may not use is as unknown as one that does not exist.
        const unknown = args.servers?.find((slug) => !state.has(slug));
        if (unknown !== undefined)
          return failure(`Unknown server "${unknown}". Call list_servers.`);
        const servers = args.servers ? [...new Set(args.servers)] : [...state.keys()];
        const hits = await searchTools(c.search, { query: args.query, limit: args.limit, servers });
        // The index may lag the catalog: a hit is shown only if the catalog still offers it.
        const tools = await Promise.all(hits.map((h) => toolFor(c, principal, h.ref)));
        const results = hits.flatMap((hit, i) => {
          const tool = tools[i];
          if (!tool) return [];
          return [
            {
              tool: tool.ref,
              title: tool.title,
              description: shortDescription(tool.description),
              server: tool.server,
              score: hit.score,
              annotations: { ...tool.annotations },
              connection: connectionOf(state, tool.server),
            },
          ];
        });
        return structured({ results });
      },
    ),
  );

  server.registerTool(
    'describe_tool',
    {
      description: DESCRIBE_DESCRIPTION,
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
    guarded(c, 'describe_tool', async ({ tool: ref }: { tool: string }) => {
      const tool = await toolFor(c, principal, ref);
      if (!tool) return failure(CALL_TOOL_TEXT.notFound(ref));
      const state = await usableServers(c, principal);
      return structured({
        tool: tool.ref,
        title: tool.title,
        description: tool.description,
        inputSchema: tool.inputSchema,
        ...(tool.outputSchema ? { outputSchema: tool.outputSchema } : {}),
        annotations: { ...tool.annotations },
        server: tool.server,
        connection: connectionOf(state, tool.server),
      });
    }),
  );

  server.registerTool(
    'call_tool',
    {
      description: CALL_DESCRIPTION,
      inputSchema: z.object({
        tool: ToolName,
        arguments: z.record(z.string(), z.unknown()).default({}),
      }),
      annotations: { readOnlyHint: false, openWorldHint: true },
    },
    guarded(c, 'call_tool', async (args: { tool: string; arguments: Record<string, unknown> }) => {
      const result = await callTool(c.gateway, {
        principal,
        ref: args.tool,
        arguments: args.arguments,
      });
      // Upstream's result goes out unchanged (the same object), or gateway's isError text.
      if (isCallToolResult(result)) return result;
      c.logger.error({ tool: 'call_tool' }, 'mcp: upstream result is not a CallToolResult');
      return failure('Internal error.');
    }),
  );
}
