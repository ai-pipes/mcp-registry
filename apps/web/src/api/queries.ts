import {
  AdminToolList,
  CatalogResponse,
  ConnectionSummary,
  ServerList,
  ServerView,
  TokenSummary,
  UserSummary,
} from '@mcp/contracts';
import { useQuery } from '@tanstack/react-query';
import { z } from 'zod';
import { api } from './client.ts';

export const queryKeys = {
  tokens: ['tokens'] as const,
  connections: ['connections'] as const,
  catalog: ['catalog'] as const,
  servers: ['servers'] as const,
  server: (slug: string) => ['servers', slug] as const,
  tools: (slug: string) => ['servers', slug, 'tools'] as const,
  users: ['users'] as const,
};

export const useTokens = () =>
  useQuery({
    queryKey: queryKeys.tokens,
    queryFn: () => api.get('/api/tokens', z.array(TokenSummary)),
  });
export const useConnections = () =>
  useQuery({
    queryKey: queryKeys.connections,
    queryFn: () => api.get('/api/connections', z.array(ConnectionSummary)),
  });
export const useCatalog = () =>
  useQuery({
    queryKey: queryKeys.catalog,
    queryFn: () => api.get('/api/catalog', CatalogResponse),
  });
export const useServers = () =>
  useQuery({ queryKey: queryKeys.servers, queryFn: () => api.get('/api/servers', ServerList) });
export const useServer = (slug: string) =>
  useQuery({
    queryKey: queryKeys.server(slug),
    queryFn: () => api.get(`/api/servers/${encodeURIComponent(slug)}`, ServerView),
  });
export const useTools = (slug: string) =>
  useQuery({
    queryKey: queryKeys.tools(slug),
    queryFn: () => api.get(`/api/servers/${encodeURIComponent(slug)}/tools`, AdminToolList),
  });
export const useUsers = () =>
  useQuery({
    queryKey: queryKeys.users,
    queryFn: () => api.get('/api/users', z.array(UserSummary)),
  });
