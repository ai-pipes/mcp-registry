import { describe, expect, it } from 'vite-plus/test';
import { SERVER_INSTRUCTIONS } from './instructions.ts';
import { shortDescription } from './text.ts';

const at = (name: string) => SERVER_INSTRUCTIONS.indexOf(name);

describe('SERVER_INSTRUCTIONS', () => {
  it('names the search → describe → call chain and the way out', () => {
    expect(at('search_tools')).toBeGreaterThan(-1);
    expect(at('search_tools')).toBeLessThan(at('describe_tool'));
    expect(at('describe_tool')).toBeLessThan(at('call_tool'));
    expect(SERVER_INSTRUCTIONS).toContain('If nothing fits, continue without them.');
  });

  it('is short and names no server: it does not grow with the catalog', () => {
    expect(SERVER_INSTRUCTIONS.length).toBeLessThan(500);
    expect(SERVER_INSTRUCTIONS).not.toMatch(/gitea|youtrack/i);
  });
});

describe('shortDescription', () => {
  it('keeps a description of up to 200 characters as is', () => {
    expect(shortDescription('Create an issue')).toBe('Create an issue');
    const exact = 'a'.repeat(200);
    expect(shortDescription(exact)).toBe(exact);
  });

  it('cuts at the last word boundary and adds «…», 200 characters at most', () => {
    const text = `${'word '.repeat(50)}tail`;
    const short = shortDescription(text);
    expect(short.length).toBeLessThanOrEqual(200);
    expect(short).toBe(`${'word '.repeat(39)}word…`);
  });

  it('drops trailing punctuation before «…» and collapses whitespace', () => {
    const text = `First sentence,\n\n   ${'second '.repeat(40)}`;
    const short = shortDescription(text);
    expect(short.startsWith('First sentence, second')).toBe(true);
    expect(short).not.toMatch(/\s{2}|\n/);
    expect(short.endsWith('second…')).toBe(true);
    const comma = shortDescription(`${'a'.repeat(190)}, ${'b'.repeat(30)}`);
    expect(comma).toBe(`${'a'.repeat(190)}…`);
  });

  it('counts and cuts by code points: an emoji at the boundary is never split', () => {
    const short = shortDescription(`${'a'.repeat(198)}😀😀😀`);
    expect(short).toBe(`${'a'.repeat(198)}😀…`);
    // encodeURIComponent throws on a lone surrogate.
    expect(() => encodeURIComponent(short)).not.toThrow();
    const words = shortDescription(`${'b'.repeat(196)} 😀 😀😀 tail`);
    expect(words).toBe(`${'b'.repeat(196)} 😀…`);
    // encodeURIComponent throws on a lone surrogate.
    expect(() => encodeURIComponent(words)).not.toThrow();
    const fits = `${'c'.repeat(199)}😀`;
    expect(shortDescription(fits)).toBe(fits);
  });

  it('cuts a single word longer than the limit hard', () => {
    const short = shortDescription('x'.repeat(500));
    expect(short).toBe(`${'x'.repeat(199)}…`);
  });
});
