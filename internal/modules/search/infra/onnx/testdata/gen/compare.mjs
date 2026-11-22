// Compares the output of cmd/embed with the reference: token ids against reference.json and vectors
// against the output of vectors.mjs, bit for bit (after rounding to float32).
//   node compare.mjs <reference.json> <node vectors.json> <go output.json>
import { readFileSync } from 'node:fs';

const [, , referencePath, nodePath, goPath] = process.argv;
const { cases } = JSON.parse(readFileSync(referencePath, 'utf8'));
const node = new Map(JSON.parse(readFileSync(nodePath, 'utf8')).map((r) => [r.i, r.vec]));
const go = JSON.parse(readFileSync(goPath, 'utf8'));
let idsEqual = 0;
let bitIdentical = 0;
let max = 0;
for (const r of go) {
  if (r.ids.length === cases[r.i].ids.length && r.ids.every((x, k) => x === cases[r.i].ids[k]))
    idsEqual++;
  const ref = node.get(r.i);
  let m = 0;
  for (let k = 0; k < r.vec.length; k++)
    m = Math.max(m, Math.abs(Math.fround(r.vec[k]) - Math.fround(ref[k])));
  if (m === 0) bitIdentical++;
  max = Math.max(max, m);
}
console.log(JSON.stringify({ texts: go.length, idsEqual, bitIdentical, maxDiff: max }));
process.exitCode = idsEqual === go.length && bitIdentical === go.length ? 0 : 1;
