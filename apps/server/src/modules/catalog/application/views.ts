import type { Server } from '../domain/server.ts';
import type { Tool } from '../domain/tool.ts';
import type { MemberServerView, ServerView, ToolView, ServerCallTarget } from './ports.ts';

/** The only mapping from an aggregate to what leaves the module: no secret, sealed or not. */
export function toServerView(server: Server, toolCount: number): ServerView {
  return {
    slug: server.slug,
    name: server.name,
    description: server.description,
    url: server.url,
    auth: server.auth,
    secretSet: server.secretSet,
    access: server.access,
    timeoutMs: server.timeoutMs,
    dailyLimit: server.dailyLimit,
    status: server.status,
    lastSyncAt: server.lastSyncAt,
    lastSyncError: server.lastSyncError,
    toolCount,
  };
}

export function toMemberServerView(server: Server, toolCount: number): MemberServerView {
  return {
    slug: server.slug,
    name: server.name,
    description: server.description,
    toolCount,
    authKind: server.auth.kind,
  };
}

export function toToolView(tool: Tool): ToolView {
  return {
    server: tool.server,
    name: tool.name,
    ref: tool.ref,
    title: tool.title,
    description: tool.description,
    inputSchema: tool.inputSchema,
    outputSchema: tool.outputSchema,
    annotations: tool.annotations,
    hidden: tool.hidden,
  };
}

/** Call parameters of a server, without its secret. */
export function toServerCallTarget(server: Server): ServerCallTarget {
  return {
    slug: server.slug,
    name: server.name,
    url: server.url,
    auth: server.auth,
    timeoutMs: server.timeoutMs,
    dailyLimit: server.dailyLimit,
  };
}
