// Generates the committed reference data of this package from the reference implementation
// (@huggingface/transformers 4.3.2 with @huggingface/tokenizers 0.2.0, as the TypeScript server
// runs it):
//   reference.json  texts with the output of the JavaScript normaliser (Precompiled.normalize) and
//                   the token ids of the full tokenizer ([text], padding, truncation: 512 cut AFTER
//                   the special tokens). Both are CPU independent.
//   pooling.json    fixed hidden states with the vector transformers.js makes of them (mean
//                   pooling, then Tensor.normalize): the arithmetic only, no model, no CPU kernels.
// Run it in the reference image (it holds transformers and the model; the repository is mounted):
//   docker run --rm --network none -v "$PWD":/repo:ro -v "$OUT":/out mcp-registry:compat-ts \
//     node /repo/internal/modules/search/infra/onnx/testdata/gen/reference.mjs \
//     /app/apps/server/node_modules/@huggingface/transformers/dist/transformers.node.mjs \
//     /app/models /repo/apps/server/test/fixtures/tools /out
import { writeFileSync } from 'node:fs';
import { curated, extra, fuzz } from './texts.mjs';

const [, , transformersPath, modelDir, fixturesDir, outDir] = process.argv;
const { env, AutoTokenizer, Tensor, mean_pooling } = await import(transformersPath);
env.localModelPath = modelDir;
env.allowLocalModels = true;
env.allowRemoteModels = false;
const tk = await AutoTokenizer.from_pretrained('Xenova/multilingual-e5-small');
// The tokenizer object is a private field of transformers.js; the normaliser is its first stage.
const privateTokenizer = '_tokenizer';
const precompiled = tk[privateTokenizer].normalizer.normalizers[0];
if (precompiled.constructor.name !== 'Precompiled') throw new Error('unexpected normalizer layout');

const hasLoneSurrogate = (s) => !s.isWellFormed();
const fuzzAll = fuzz();
const fuzzSample = fuzzAll.filter((_, i) => i % 6 === 0);
const groups = [
  ['curated', curated(fixturesDir)],
  ['extra', extra],
  ['fuzz', fuzzSample],
];
const cases = [];
let excluded = 0;
for (const [group, texts] of groups) {
  for (const text of texts) {
    // A lone surrogate cannot be a Go string (and so cannot reach the Go embedder): left out.
    if (hasLoneSurrogate(text)) {
      excluded++;
      continue;
    }
    const ids = Array.from(tk([text], { padding: true, truncation: true }).input_ids.data, Number);
    cases.push({ group, text, normalized: precompiled.normalize(text), ids });
  }
}
writeFileSync(
  `${outDir}/reference.json`,
  JSON.stringify({
    transformers: '4.3.2',
    tokenizers: '0.2.0',
    node: process.version,
    unicode: process.versions.unicode,
    excludedLoneSurrogates: excluded,
    cases,
  }),
);

// Pooling: deterministic hidden states (float32), several lengths, dimension 384.
let seed = 20261122;
const rnd = () => {
  seed = (seed + 0x6d2b79f5) >>> 0;
  let t = seed;
  t = Math.imul(t ^ (t >>> 15), t | 1);
  t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
  return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
};
const b64 = (f32) => Buffer.from(f32.buffer, f32.byteOffset, f32.byteLength).toString('base64');
const dim = 384;
const poolCases = [];
for (const length of [1, 2, 5, 17, 40]) {
  const hidden = new Float32Array(length * dim);
  for (let i = 0; i < hidden.length; i++) {
    // mostly small values, a few large and a few tiny ones, so rounding differences would show
    const scale = rnd() < 0.05 ? 50 : rnd() < 0.05 ? 1e-4 : 1;
    hidden[i] = (rnd() * 2 - 1) * scale;
  }
  const tensor = new Tensor('float32', hidden, [1, length, dim]);
  const mask = new Tensor('int64', new BigInt64Array(length).fill(1n), [1, length]);
  const vector = mean_pooling(tensor, mask).normalize(2, -1).data;
  poolCases.push({ length, dim, hidden: b64(hidden), vector: b64(new Float32Array(vector)) });
}
writeFileSync(
  `${outDir}/pooling.json`,
  JSON.stringify({ encoding: 'base64 little-endian float32', cases: poolCases }),
);
console.log(JSON.stringify({ cases: cases.length, excluded, pooling: poolCases.length }));
