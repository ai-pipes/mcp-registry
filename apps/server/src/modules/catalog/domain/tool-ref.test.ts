import { describe, expect, it } from 'vite-plus/test';
import { ToolRef } from './tool-ref.ts';

describe('ToolRef', () => {
  it('splits on the first dot only', () => {
    expect(ToolRef.parse('gitea.issues.create')).toEqual({
      ok: true,
      value: { server: 'gitea', tool: 'issues.create' },
    });
  });

  it('accepts dashes and underscores in the tool name', () => {
    expect(ToolRef.parse('youtrack.get-file_contents')).toEqual({
      ok: true,
      value: { server: 'youtrack', tool: 'get-file_contents' },
    });
  });

  it.each(['gitea', '.x', 'gitea.', '', 'Gitea.x'])('rejects %j', (raw) => {
    const r = ToolRef.parse(raw);
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.error.code).toBe('invalid_tool_ref');
  });

  it('formats server and tool', () => {
    expect(ToolRef.format('gitea', 'issues.create')).toBe('gitea.issues.create');
    expect(ToolRef.parse(ToolRef.format('a1', 'x.y.z'))).toMatchObject({
      ok: true,
      value: { server: 'a1', tool: 'x.y.z' },
    });
  });
});
