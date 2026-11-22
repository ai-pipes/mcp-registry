// Regenerates formencode.json and basic.json (the reference's form encoding of Basic credentials).
// Run from this directory: node gen/gen.mjs   (writes ../formencode.json and ../basic.json)
import { writeFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
const target = (name) => fileURLToPath(new URL('../' + name, import.meta.url));

const fixed = [
  '',
  ' ',
  'a b',
  'gate+way%2F:secret',
  'gateway-client',
  'a*b',
  'a~b',
  '-._*~',
  "!'()",
  '~~~',
  '%41',
  'a&b=c',
  'é',
  '日本語',
  '😀',
  '\u0000',
  '\n',
  '\t',
  'a/b?c#d',
  'UPPER lower 0123456789',
  '@:;,$[]{}<>"\\^`|',
  'ÿĀ',
  'uuid 7f3c2a10-aaaa-4bbb-8ccc-0123456789ab',
  'p@ss w0rd!*~',
  'ключ',
  'a+b',
  '100%',
];
let seed = 12345;
const rnd = () => (seed = (seed * 1103515245 + 12345) & 0x7fffffff) / 0x7fffffff;
const pool = [];
for (let i = 0x20; i < 0x7f; i++) pool.push(String.fromCharCode(i));
pool.push('é', 'ß', '日', '本', 'я', '€', '😀', '🎉', '\u0000', '\n', '\r', '\t', ' ', ' ', '﻿');
const out = [...fixed];
while (out.length < 130) {
  const n = 1 + Math.floor(rnd() * 16);
  let s = '';
  for (let i = 0; i < n; i++) s += pool[Math.floor(rnd() * pool.length)];
  out.push(s);
}
const rows = out.map((s) => ({ in: s, out: new URLSearchParams({ v: s }).toString().slice(2) }));
writeFileSync(target('formencode.json'), JSON.stringify(rows, null, 1) + '\n');

const fe2 = (v) => new URLSearchParams({ v }).toString().slice(2);
const pairs = [
  ['gateway-client', 'gate+way%2F:secret'],
  ['7f3c2a10-aaaa-4bbb-8ccc-0123456789ab', 's3cr3t'],
  ['id*star', 'sec*ret'],
  ['id~tilde', 'sec~ret'],
  ['a b', 'c d'],
  ['client:colon', 'se:cret'],
  ['é日', 'ключ😀'],
  ['id', 'p@ss/w0rd=='],
  ['x', 'a&b=c%d'],
  ['', ''],
  ['id.dot_under-dash', "!'()"],
  ['cid', '~*-._'],
];
writeFileSync(
  target('basic.json'),
  JSON.stringify(
    pairs.map(([id, secret]) => ({
      id,
      secret,
      header: 'Basic ' + Buffer.from(`${fe2(id)}:${fe2(secret)}`, 'utf8').toString('base64'),
    })),
    null,
    1,
  ) + '\n',
);
