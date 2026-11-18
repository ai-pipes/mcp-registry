// Exports every zod schema of the contracts package as a JSON Schema file (draft 2020-12, output
// side), one file per export: <outDir>/<ExportName>.json. The Go compat suite validates REST
// responses with these files. Usage:
//   node --experimental-strip-types packages/contracts/scripts/export-json-schema.mjs compat/schemas
import { mkdirSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import { basename, resolve } from 'node:path';
import { z } from 'zod';
import * as contracts from '../src/index.ts';

const outDir = process.argv[2];
if (!outDir) {
  console.error('usage: export-json-schema.mjs <outDir>');
  process.exit(2);
}

const isSchema = (v) => typeof v === 'object' && v !== null && '_zod' in v;

// Sorted keys at every level so the files diff cleanly.
function sortKeys(v) {
  if (Array.isArray(v)) return v.map(sortKeys);
  if (v && typeof v === 'object') {
    return Object.fromEntries(
      Object.keys(v)
        .toSorted()
        .map((k) => [k, sortKeys(v[k])]),
    );
  }
  return v;
}

const dir = resolve(outDir);
// The directory is emptied of *.json files below: refuse anything but a schemas directory.
if (basename(dir) !== 'schemas') {
  console.error(`refusing to write into ${dir}: the directory must be named "schemas"`);
  process.exit(2);
}
mkdirSync(dir, { recursive: true });
for (const f of readdirSync(dir)) if (f.endsWith('.json')) rmSync(resolve(dir, f));

const names = [];
for (const [name, value] of Object.entries(contracts).toSorted(([a], [b]) => a.localeCompare(b))) {
  if (!isSchema(value)) continue;
  let schema;
  try {
    schema = z.toJSONSchema(value, { target: 'draft-2020-12', io: 'output' });
  } catch (error) {
    console.error(
      `cannot convert ${name}: ${error instanceof Error ? error.message : String(error)}`,
    );
    process.exit(1);
  }
  writeFileSync(resolve(dir, `${name}.json`), `${JSON.stringify(sortKeys(schema), null, 2)}\n`);
  names.push(name);
}
console.log(`exported ${names.length} schemas to ${outDir}: ${names.join(', ')}`);
