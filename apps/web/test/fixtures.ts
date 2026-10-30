import type { ServerView } from '@mcp/contracts';

/** A `ServerView` as the admin REST returns it; tests override what they care about. */
export function serverView(overrides: Partial<ServerView> = {}): ServerView {
  return {
    slug: 'gitea',
    name: 'Gitea',
    description: 'Git hosting',
    url: 'https://gitea.example.com/mcp',
    auth: { kind: 'none' },
    secretSet: false,
    access: { kind: 'all' },
    timeoutMs: 120_000,
    dailyLimit: null,
    status: 'active',
    lastSyncAt: null,
    lastSyncError: null,
    toolCount: 0,
    ...overrides,
  };
}
