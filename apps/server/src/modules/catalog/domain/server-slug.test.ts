import { describe, expect, it } from 'vite-plus/test';
import { ServerSlug } from './server-slug.ts';

describe('ServerSlug.parse', () => {
  it('accepts kebab-case 2-32 chars starting with a letter', () => {
    expect(ServerSlug.parse('gitea')).toEqual({ ok: true, value: 'gitea' });
    expect(ServerSlug.parse('my-server-2')).toMatchObject({ ok: true });
    expect(ServerSlug.parse('a'.repeat(32))).toMatchObject({ ok: true });
  });

  it.each(['Gitea', 'g', '', '1abc', '-ab', 'a b', 'a.b', 'a_b', 'a'.repeat(33)])(
    'rejects %j',
    (raw) => {
      const r = ServerSlug.parse(raw);
      expect(r.ok).toBe(false);
      if (!r.ok) expect(r.error.code).toBe('invalid_slug');
    },
  );
});
