import { describe, expect, it } from 'vite-plus/test';
import { redirectTo } from './navigate.ts';

describe('redirectTo', () => {
  it.each(['javascript:alert(1)', 'data:text/html,x', '/relative', '//host'])(
    'refuses %s',
    (url) => {
      expect(() => redirectTo(url)).toThrow();
    },
  );
});
