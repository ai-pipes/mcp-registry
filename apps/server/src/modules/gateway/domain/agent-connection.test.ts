import { describe, expect, it } from 'vite-plus/test';
import { AgentConnection } from './agent-connection.ts';

describe('AgentConnection', () => {
  it('servers that use no personal credentials need no connection', () => {
    expect(AgentConnection.of('none', undefined)).toBe('not_required');
    expect(AgentConnection.of('shared_secret', 'missing')).toBe('not_required');
  });

  it('personal servers follow the connection state; no record means needs_connection', () => {
    for (const kind of ['oauth_user', 'pat_user'] as const) {
      expect(AgentConnection.of(kind, 'ready')).toBe('ready');
      expect(AgentConnection.of(kind, 'expired')).toBe('expired');
      expect(AgentConnection.of(kind, 'missing')).toBe('needs_connection');
      expect(AgentConnection.of(kind, undefined)).toBe('needs_connection');
    }
  });

  it('only a missing or expired connection gets the link to the connections page', () => {
    expect(AgentConnection.needsLink('needs_connection')).toBe(true);
    expect(AgentConnection.needsLink('expired')).toBe(true);
    expect(AgentConnection.needsLink('ready')).toBe(false);
    expect(AgentConnection.needsLink('not_required')).toBe(false);
  });
});
