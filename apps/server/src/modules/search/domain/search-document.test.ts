import { describe, expect, it } from 'vite-plus/test';
import { buildPassage, MAX_PASSAGE_CHARS, schemaText } from './search-document.ts';

const base = {
  serverName: 'YouTrack',
  serverDescription: 'Issue tracker',
  toolName: 'create_issue',
  title: 'Create issue',
  description: 'Creates a new issue.',
};

describe('buildPassage', () => {
  it('joins server, tool name with title, and description', () => {
    expect(buildPassage(base)).toBe(
      'YouTrack. Issue tracker\ncreate_issue Create issue\nCreates a new issue.',
    );
  });

  it('omits an absent title without a stray space', () => {
    expect(buildPassage({ ...base, title: null })).toBe(
      'YouTrack. Issue tracker\ncreate_issue\nCreates a new issue.',
    );
  });

  it('truncates to MAX_PASSAGE_CHARS', () => {
    expect(buildPassage({ ...base, description: 'x'.repeat(5000) })).toHaveLength(
      MAX_PASSAGE_CHARS,
    );
  });
});

describe('schemaText', () => {
  it('lists parameter names and their descriptions, nested ones included', () => {
    const schema = {
      type: 'object',
      properties: {
        title: { type: 'string', description: 'Issue title' },
        labels: { type: 'array', items: { type: 'object', properties: { color: {} } } },
        owner: { type: 'object', properties: { login: { description: 'User login' } } },
      },
    };
    expect(schemaText(schema)).toBe('title Issue title labels color owner login User login');
  });

  it('is empty for a schema without properties or with unexpected shapes', () => {
    expect(schemaText({ type: 'object' })).toBe('');
    expect(schemaText({ properties: 'nope' })).toBe('');
    expect(schemaText({ properties: { a: null, b: 5 } })).toBe('a b');
  });

  it('is capped', () => {
    const properties = Object.fromEntries(
      Array.from({ length: 500 }, (_, i) => [`param_${i}`, { description: 'd'.repeat(20) }]),
    );
    expect(schemaText({ properties }).length).toBeLessThanOrEqual(MAX_PASSAGE_CHARS);
  });
});
