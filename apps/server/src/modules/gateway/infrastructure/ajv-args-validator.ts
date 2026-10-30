import { createHash } from 'node:crypto';
import type { JsonSchemaValidator } from '@modelcontextprotocol/client';
import { AjvJsonSchemaValidator } from '@modelcontextprotocol/client/validators/ajv';
import type { ArgsCheck, ArgsValidator } from '../application/ports.ts';

/** Upper bound on cached compiled schemas (one per upstream tool in practice). */
const MAX_CACHED = 2000;

// The SDK reports `data<instancePath> <message>, data<instancePath> <message>…` (ajv errorsText).
// Messages start with "must"; the path is everything between `data` and the first ` must`.
const FIRST_ERROR = /^data(.*?) (must\b.*?)(?:, data(?:\/| must\b).*)?$/s;

type Compiled = JsonSchemaValidator<unknown> | 'unchecked';
type Json = Record<string, unknown>;

interface ValidatorLogger {
  warn(fields: Record<string, unknown>, message: string): void;
}

const isObject = (v: unknown): v is Json =>
  typeof v === 'object' && v !== null && !Array.isArray(v);

// Where a JSON Schema holds subschemas. Keys of `properties` & co. are field names, not keywords.
const ONE_SCHEMA = [
  'additionalProperties',
  'additionalItems',
  'unevaluatedProperties',
  'unevaluatedItems',
  'items',
  'contains',
  'propertyNames',
  'not',
  'if',
  'then',
  'else',
  'contentSchema',
];
const SCHEMA_LIST = ['allOf', 'anyOf', 'oneOf', 'prefixItems', 'items'];
const SCHEMA_MAP = ['properties', '$defs', 'definitions', 'dependentSchemas', 'dependencies'];

const mapValues = (o: Json, f: (v: unknown) => unknown): Json =>
  Object.fromEntries(Object.entries(o).map(([k, v]) => [k, f(v)]));

/**
 * A copy of the schema without regex keywords (`pattern`, `patternProperties`), walking only
 * schema positions. Upstream schemas would otherwise run their regexes, compiled to native
 * RegExp, on agent input in the one event loop (ReDoS). Validation here is advisory: upstream
 * enforces the full schema. With `patternProperties` gone, the sibling `additionalProperties` /
 * `unevaluatedProperties` go too, or fields the patterns allowed would be refused.
 */
function withoutRegex(node: unknown): unknown {
  if (!isObject(node)) return node;
  const out: Json = { ...node };
  delete out.pattern;
  if ('patternProperties' in out) {
    delete out.patternProperties;
    delete out.additionalProperties;
    delete out.unevaluatedProperties;
  }
  for (const k of ONE_SCHEMA) if (isObject(out[k])) out[k] = withoutRegex(out[k]);
  for (const k of SCHEMA_LIST) {
    const list = out[k];
    if (Array.isArray(list)) out[k] = list.map(withoutRegex);
  }
  for (const k of SCHEMA_MAP) {
    const map = out[k];
    if (isObject(map)) out[k] = mapValues(map, withoutRegex);
  }
  return out;
}

/** What is compiled: no regex keywords, and no top-level `$id` (see `compiled`). */
function prepared(schema: Json): Json {
  const copy = withoutRegex(schema);
  if (!isObject(copy)) return {};
  delete copy.$id;
  return copy;
}

function toolOf(ref: string): { server: string; tool: string } {
  const dot = ref.indexOf('.');
  return dot < 0
    ? { server: '', tool: ref }
    : { server: ref.slice(0, dot), tool: ref.slice(dot + 1) };
}

/**
 * ArgsValidator on the MCP SDK's JSON Schema validator (ajv bundled in the SDK; it picks the
 * engine by the schema's `$schema`: 2020-12 by default, 2019-09, draft-07/06). Compiled schemas
 * are cached by a hash of the schema, so a tool is compiled once until upstream changes it.
 * The top-level `$id` is dropped before compiling: with it the SDK would return a validator
 * compiled earlier for that `$id` (stale after an upstream change, or another tool's).
 * A schema the validator cannot compile is not enforced here (upstream validates it itself);
 * that is logged once per schema.
 */
export class AjvArgsValidator implements ArgsValidator {
  private readonly sdk = new AjvJsonSchemaValidator();
  private readonly cache = new Map<string, Compiled>();
  /** For tests: how many schemas were compiled. */
  compiledCount = 0;

  constructor(private readonly logger: ValidatorLogger) {}

  validate(ref: string, schema: Json, args: unknown): ArgsCheck {
    const check = this.compiled(ref, schema);
    if (check === 'unchecked') return { ok: true };
    const r = check(args);
    if (r.valid) return { ok: true };
    const m = FIRST_ERROR.exec(r.errorMessage);
    if (!m) return { ok: false, path: '/', message: r.errorMessage };
    return { ok: false, path: m[1] || '/', message: m[2] ?? r.errorMessage };
  }

  private compiled(ref: string, schema: Json): Compiled {
    const key = createHash('sha256').update(JSON.stringify(schema)).digest('hex');
    const hit = this.cache.get(key);
    if (hit !== undefined) return hit;
    let compiled: Compiled;
    try {
      compiled = this.sdk.getValidator(prepared(schema));
    } catch (error) {
      compiled = 'unchecked';
      // The compile error describes the schema (upstream metadata); arguments never reach it.
      const message = error instanceof Error ? error.message : String(error);
      this.logger.warn(
        { ...toolOf(ref), error: message.slice(0, 500) },
        'gateway: tool input schema not compiled, not validating',
      );
    }
    this.compiledCount += 1;
    if (this.cache.size >= MAX_CACHED) {
      const oldest = this.cache.keys().next();
      if (!oldest.done) this.cache.delete(oldest.value);
    }
    this.cache.set(key, compiled);
    return compiled;
  }
}
