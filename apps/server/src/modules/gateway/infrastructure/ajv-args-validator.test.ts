import { describe, expect, it } from 'vite-plus/test';
import { AjvArgsValidator } from './ajv-args-validator.ts';

type Warned = { fields: Record<string, unknown>; message: string };
let warned: Warned[] = [];
const logger = {
  warn: (fields: Record<string, unknown>, message: string) => {
    warned.push({ fields, message });
  },
};
const REF = 'gitea.create_issue';

const schema = {
  type: 'object',
  properties: {
    title: { type: 'string' },
    labels: { type: 'array', items: { type: 'integer' } },
    meta: { type: 'object', properties: { 'due date': { type: 'string', format: 'date' } } },
  },
  required: ['title'],
  additionalProperties: false,
};

describe('AjvArgsValidator (the MCP SDK JSON Schema validator)', () => {
  const v = new AjvArgsValidator(logger);

  it('accepts matching arguments', () => {
    expect(v.validate(REF, schema, { title: 'Bug', labels: [1, 2] })).toEqual({ ok: true });
  });

  it('reports the JSON pointer of the first bad field and the reason', () => {
    expect(v.validate(REF, schema, { title: 7 })).toEqual({
      ok: false,
      path: '/title',
      message: 'must be string',
    });
    expect(v.validate(REF, schema, { title: 'x', labels: [1, 'two'] })).toEqual({
      ok: false,
      path: '/labels/1',
      message: 'must be integer',
    });
  });

  it('a missing required field or an unknown one is reported at the root', () => {
    expect(v.validate(REF, schema, {})).toEqual({
      ok: false,
      path: '/',
      message: "must have required property 'title'",
    });
    expect(v.validate(REF, schema, { title: 'x', extra: 1 })).toEqual({
      ok: false,
      path: '/',
      message: 'must NOT have additional properties',
    });
  });

  it('field names with spaces keep their whole path; formats are checked', () => {
    expect(v.validate(REF, schema, { title: 'x', meta: { 'due date': 'soon' } })).toEqual({
      ok: false,
      path: '/meta/due date',
      message: 'must match format "date"',
    });
  });

  it('honours the declared dialect (draft-07 schemas from older upstreams)', () => {
    const draft7 = {
      $schema: 'http://json-schema.org/draft-07/schema#',
      type: 'object',
      properties: { n: { type: 'number' } },
    };
    expect(v.validate(REF, draft7, { n: 1 })).toEqual({ ok: true });
    expect(v.validate(REF, draft7, { n: 'x' })).toMatchObject({ ok: false, path: '/n' });
  });

  it('caches the compiled schema: the same schema object or an equal copy is compiled once', () => {
    const counting = new AjvArgsValidator(logger);
    for (let i = 0; i < 3; i++) counting.validate(REF, structuredClone(schema), { title: 'x' });
    expect(counting.compiledCount).toBe(1);
    counting.validate(REF, { type: 'object' }, {});
    expect(counting.compiledCount).toBe(2);
  });

  it('a schema it cannot compile is left to upstream, with one warning per schema', () => {
    warned = [];
    const fresh = new AjvArgsValidator(logger);
    const unknownDialect = { $schema: 'https://example.com/my-dialect', type: 'object' };
    expect(fresh.validate(REF, unknownDialect, { anything: 'secret arg' })).toEqual({ ok: true });
    expect(fresh.validate(REF, unknownDialect, { anything: 1 })).toEqual({ ok: true });
    expect(warned).toHaveLength(1);
    expect(warned[0]?.message).toBe('gateway: tool input schema not compiled, not validating');
    expect(warned[0]?.fields).toMatchObject({ server: 'gitea', tool: 'create_issue' });
    expect(typeof warned[0]?.fields.error).toBe('string');
    expect(JSON.stringify(warned)).not.toContain('secret arg');
  });

  it('regex keywords are not run: a catastrophic pattern returns at once (upstream enforces it)', () => {
    const evil = {
      type: 'object',
      properties: { name: { type: 'string', pattern: '^(a+)+$' } },
      patternProperties: { '^(x+)+y$': { type: 'integer' } },
      additionalProperties: false,
    };
    const input = `${'a'.repeat(40)}!`;
    const started = performance.now();
    expect(v.validate(REF, evil, { name: input, ['x'.repeat(40)]: 'z' })).toEqual({
      ok: true,
    });
    expect(performance.now() - started).toBeLessThan(500);
    // Everything else in the schema is still checked.
    expect(v.validate(REF, evil, { name: 5 })).toMatchObject({ ok: false, path: '/name' });
  });

  it('only schema positions are stripped: a property named "pattern" is still validated', () => {
    const s = {
      type: 'object',
      properties: {
        pattern: { type: 'string' },
        patternProperties: { type: 'integer' },
        nested: { type: 'array', items: { anyOf: [{ type: 'string', pattern: '^(a+)+$' }] } },
      },
      required: ['pattern'],
      $defs: { d: { type: 'string', pattern: '^(a+)+$' } },
    };
    expect(v.validate(REF, s, {})).toEqual({
      ok: false,
      path: '/',
      message: "must have required property 'pattern'",
    });
    expect(v.validate(REF, s, { pattern: 1 })).toMatchObject({ ok: false, path: '/pattern' });
    expect(v.validate(REF, s, { pattern: 'x', patternProperties: 'y' })).toMatchObject({
      ok: false,
      path: '/patternProperties',
    });
    expect(v.validate(REF, s, { pattern: 'x', nested: [`${'a'.repeat(40)}!`] })).toEqual({
      ok: true,
    });
    expect(s.properties.nested.items.anyOf[0]).toHaveProperty('pattern');
  });

  it('a changed schema with the same $id takes effect (no stale compiled $id)', () => {
    const fresh = new AjvArgsValidator(logger);
    const v1 = { $id: 'urn:tool', type: 'object', properties: { n: { type: 'string' } } };
    const v2 = { $id: 'urn:tool', type: 'object', properties: { n: { type: 'number' } } };
    expect(fresh.validate(REF, v1, { n: 1 })).toMatchObject({ ok: false });
    expect(fresh.validate(REF, v2, { n: 1 })).toEqual({ ok: true });
    expect(fresh.validate('other.tool', v2, { n: 'x' })).toMatchObject({ ok: false, path: '/n' });
  });

  it('a field name containing " must" is cut at the first " must" (known parse limit)', () => {
    const odd = { type: 'object', properties: { 'x must y': { type: 'string' } } };
    expect(v.validate(REF, odd, { 'x must y': 1 })).toEqual({
      ok: false,
      path: '/x',
      message: 'must y must be string',
    });
  });
});
