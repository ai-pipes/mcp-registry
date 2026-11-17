import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vite-plus/test';
import { VERSION } from './version.ts';

describe('VERSION', () => {
  it('is the version field of apps/server/package.json', () => {
    const pkg: unknown = JSON.parse(
      readFileSync(new URL('../../package.json', import.meta.url), 'utf8'),
    );
    expect(pkg).toMatchObject({ version: VERSION });
    expect(VERSION).toMatch(/^\d+\.\d+\.\d+/);
  });
});
