import { readdir, readFile } from 'node:fs/promises';
import { join } from 'node:path';
import { parseSync } from '@swc/core';

/** The module schemas. `platform.*` is shared and not listed: every module may use it. */
const MODULE_SCHEMAS = ['identity', 'catalog', 'connections', 'search', 'gateway'];
const SCHEMA_REF = new RegExp(String.raw`\b(${MODULE_SCHEMAS.join('|')})\.([a-z_]+)`, 'g');
/** A plain string or untagged template counts as SQL when it reads like a statement. */
const SQL_WORDS =
  /\b(select|insert\s+into|update|delete\s+from|join|create\s+(table|index)|alter\s+table|truncate)\b/i;

export type SchemaViolation = { file: string; module: string; ref: string };

type Node = { type?: unknown; [key: string]: unknown };

const isNode = (v: unknown): v is Node => v !== null && typeof v === 'object';

function templateText(template: unknown): string {
  if (!isNode(template) || !Array.isArray(template.quasis)) return '';
  return template.quasis
    .map((q: unknown) => (isNode(q) && typeof q.raw === 'string' ? q.raw : ''))
    .join(' ');
}

function literalText(node: unknown): string | null {
  if (!isNode(node)) return null;
  if (node.type === 'StringLiteral' && typeof node.value === 'string') return node.value;
  if (node.type === 'TemplateLiteral') return templateText(node);
  return null;
}

/**
 * SQL text in a source file: every tagged template (postgres.js `sql\`…\``, `tx\`…\``), every
 * literal handed to `.unsafe(…)`, and any other string or template that reads like SQL.
 * Interpolated expressions are not SQL text (they are bound parameters) but are searched for
 * nested SQL templates.
 */
function sqlTexts(source: string): string[] {
  const out: string[] = [];
  const walk = (node: unknown): void => {
    if (Array.isArray(node)) {
      for (const n of node) walk(n);
      return;
    }
    if (!isNode(node)) return;
    if (node.type === 'TaggedTemplateExpression' && isNode(node.template)) {
      out.push(templateText(node.template));
      walk(node.tag);
      walk(node.template.expressions);
      return;
    }
    if (node.type === 'CallExpression' && isNode(node.callee)) {
      const property = node.callee.property;
      if (isNode(property) && property.value === 'unsafe' && Array.isArray(node.arguments)) {
        for (const arg of node.arguments) {
          const text = isNode(arg) ? literalText(arg.expression) : null;
          if (text !== null) out.push(text);
        }
      }
    }
    const text = literalText(node);
    if (text !== null && SQL_WORDS.test(text)) out.push(text);
    for (const [key, value] of Object.entries(node)) if (key !== 'span') walk(value);
  };
  walk(parseSync(source, { syntax: 'typescript', target: 'es2022' }).body);
  return out;
}

async function sourceFiles(dir: string): Promise<string[]> {
  const entries = await readdir(dir, { withFileTypes: true, recursive: true });
  return entries
    .filter((e) => e.isFile() && e.name.endsWith('.ts') && !e.name.endsWith('.test.ts'))
    .map((e) => join(e.parentPath, e.name));
}

/**
 * Each module owns its schema: the SQL in `<modulesDir>/<module>/**` (tests aside) names no other
 * module's schema, not even in a join. `platform.*` is shared.
 */
export async function foreignSchemaRefs(modulesDir: string): Promise<SchemaViolation[]> {
  const modules = (await readdir(modulesDir, { withFileTypes: true })).filter((e) =>
    e.isDirectory(),
  );
  const found = await Promise.all(
    modules.map(async ({ name: module }) => {
      const files = await sourceFiles(join(modulesDir, module));
      const perFile = await Promise.all(
        files.map(async (file) => {
          const texts = sqlTexts(await readFile(file, 'utf8'));
          return texts.flatMap((text) =>
            [...text.matchAll(SCHEMA_REF)]
              .filter((m) => m[1] !== module)
              .map((m) => ({ file, module, ref: m[0] })),
          );
        }),
      );
      return perFile.flat();
    }),
  );
  return found.flat();
}
