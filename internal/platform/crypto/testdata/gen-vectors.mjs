// Generates ts-vectors.json: values sealed by the real TypeScript AesGcmSecretBox, so the Go
// implementation is proven byte-compatible with it. Run from the repository root (Node 22.18+
// strips TypeScript types by default; older 22.x needs --experimental-strip-types):
//
//   node internal/platform/crypto/testdata/gen-vectors.mjs
//
// The keys are random test keys made here on every run and are never real keys. The output is
// an object with named sections ("secretbox", "keyCheck"); add a new section by adding a key to
// `sections` below, existing sections are kept as they are when the file already has them.
import { randomBytes } from 'node:crypto';
import { readFileSync, writeFileSync } from 'node:fs';
import { AesGcmSecretBox } from '../../../../apps/server/src/platform/crypto/secret-box.ts';

const out = new URL('./ts-vectors.json', import.meta.url);
const b64 = (bytes) => Buffer.from(bytes).toString('base64');

function secretboxSection() {
  const key = randomBytes(32);
  const previous = randomBytes(32);
  const box = new AesGcmSecretBox(key);
  const old = new AesGcmSecretBox(previous);
  const plains = [
    '',
    'sk_test_0123456789abcdef',
    'pa$$word with spaces and "quotes"',
    'Ключ секретный: 密钥 🔐',
    'x'.repeat(1000),
  ];
  return {
    layout: 'iv(12) | tag(16) | ciphertext, all base64',
    key: b64(key),
    previousKey: b64(previous),
    vectors: plains.map((plain) => ({ plain, sealed: b64(box.seal(plain)) })),
    sealedWithPrevious: {
      plain: 'sealed with the previous key',
      sealed: b64(old.seal('sealed with the previous key')),
    },
  };
}

// The check text of apps/server/src/platform/crypto/key-check.ts (PLAIN, not exported there). The
// Go test keycheck_test.go reads that source file and asserts the same literal is in it.
const KEY_CHECK_PLAIN = 'mcp-registry key check v1';

function keyCheckSection() {
  const key = randomBytes(32);
  return {
    key: b64(key),
    plain: KEY_CHECK_PLAIN,
    sealed: b64(new AesGcmSecretBox(key).seal(KEY_CHECK_PLAIN)),
  };
}

// Named sections. Keep an existing section unless it is regenerated explicitly by name:
//   node gen-vectors.mjs secretbox
const sections = { secretbox: secretboxSection, keyCheck: keyCheckSection };

let existing = {};
try {
  existing = JSON.parse(readFileSync(out, 'utf8'));
} catch {
  // first run
}
const only = process.argv.slice(2);
for (const [name, make] of Object.entries(sections)) {
  if (only.length === 0 ? !(name in existing) : only.includes(name)) existing[name] = make();
}
writeFileSync(out, JSON.stringify(existing, null, 2) + '\n');
