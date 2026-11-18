// Generates the reference embeddings of compat/golden/embeddings-<arch>.json: for 110 texts (the
// passages of the Gitea and YouTrack fixtures, queries, odd Unicode and two very long texts) the
// token ids and the L2-normalised mean-pooled vector of Xenova/multilingual-e5-small (q8) as
// @huggingface/transformers computes them. The Go embedder (milestone 4) is judged against them.
//
// Run it inside node:22-bookworm-slim of the TARGET architecture (the q8 model gives slightly
// different vectors on amd64 and arm64), with @huggingface/transformers@4.3.2 installed:
//
//   node gen-embeddings.mjs <path to transformers entry> <model directory> <repo root> <out.json>
//
// <model directory> holds Xenova/multilingual-e5-small (the image's baked-in copy, or one fetched by
// scripts/download-model.mjs). <repo root> is this repository (the fixtures are read from
// apps/server/test/fixtures/tools). Use --platform linux/amd64 or linux/arm64 with docker run.
import { readFileSync, writeFileSync } from 'node:fs';
const [,, tfPath, modelDir, repo, out] = process.argv;
const { env, pipeline } = await import(tfPath);
env.localModelPath = modelDir; env.cacheDir = modelDir; env.allowLocalModels = true; env.allowRemoteModels = false;
const ex = await pipeline('feature-extraction', 'Xenova/multilingual-e5-small', { dtype: 'q8' });
const texts = [];
for (const f of ['gitea', 'youtrack']) {
  const j = JSON.parse(readFileSync(`${repo}/apps/server/test/fixtures/tools/${f}.json`, 'utf8'));
  const tools = j.tools ?? j;
  for (const t of tools) texts.push(`passage: ${f}. ${f} server\n${t.name} ${t.title ?? ''}\n${t.description ?? ''}`);
}
const queries = ['create an issue', 'list pull requests in a repository', 'найти задачу по номеру', 'создать задачу в трекере',
  'what is the weather in Paris', 'book a flight to Tokyo', 'コードレビューを依頼する', '搜索仓库中的文件', 'merge a branch 🚀🔥',
  'a  b   c    multiple     spaces', '  leading and trailing  ', 'Ünïcödé façade naïve Ærøskøbing', 'tabs\tand\nnewlines\r\nhere',
  'ＦＵＬＬＷＩＤＴＨ ｔｅｘｔ ①②③', 'é combining vs é precomposed', 'x', '', 'user@example.com https://example.org/a?b=c&d=e#f',
  'SELECT * FROM t WHERE a <> 1; -- comment', 'مرحبا بالعالم', 'שלום עולם', 'नमस्ते दुनिया', '안녕하세요 세계', 'zero​width nbsp﻿bom'];
for (const q of queries) texts.push(`query: ${q}`);
texts.push('passage: ' + 'The quick brown fox jumps over the lazy dog. Быстрая лиса. '.repeat(120));
texts.push('query: ' + 'длинный запрос про репозиторий и задачи '.repeat(200));
const rows = [];
const lat = [];
for (const text of texts) {
  const enc = ex.tokenizer([text], { padding: true, truncation: true });
  const ids = Array.from(enc.input_ids.data, (x) => Number(x));
  const t0 = performance.now();
  const o = await ex([text], { pooling: 'mean', normalize: true });
  lat.push(performance.now() - t0);
  rows.push({ text, ids, vec: o.tolist()[0] });
}
writeFileSync(out, JSON.stringify(rows));
lat.sort((a, b) => a - b);
console.log(`texts=${rows.length} p50=${lat[Math.floor(lat.length / 2)].toFixed(1)}ms p95=${lat[Math.floor(lat.length * 0.95)].toFixed(1)}ms rss=${(process.memoryUsage().rss / 1e6).toFixed(0)}MB maxTokens=${Math.max(...rows.map((r) => r.ids.length))}`);
