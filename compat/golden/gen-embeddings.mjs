// Embeds texts with Xenova/multilingual-e5-small (q8) exactly as @huggingface/transformers computes
// them: the token ids and the L2-normalised mean-pooled vector, one text per call. The suite runs it
// at test time inside a container of the REFERENCE image (compat/search_vectors_test.go, no network):
// q8 vectors differ between CPUs of the same architecture by up to about 0.015, so a reference is
// only meaningful when it comes from the machine that runs the comparison.
//
//   node gen-embeddings.mjs <path to transformers entry> <model directory> --stdin -
//     reads a JSON array of texts from stdin, writes the JSON rows [{text, ids, vec}] to stdout.
//   node gen-embeddings.mjs <path to transformers entry> <model directory> <repo root> <out.json>
//     embeds a fixed list of 110 texts (the passages of the Gitea and YouTrack fixtures read from
//     <repo root>/apps/server/test/fixtures/tools, queries, odd Unicode, two very long texts); kept
//     as the query-side reference for the Go embedder's unit tests of a later milestone.
//
// <model directory> holds Xenova/multilingual-e5-small (the image's /app/models, or one fetched by
// scripts/download-model.mjs).
import { readFileSync, writeFileSync } from 'node:fs';
const [,, tfPath, modelDir, repo, out] = process.argv;
const { env, pipeline } = await import(tfPath);
env.localModelPath = modelDir; env.cacheDir = modelDir; env.allowLocalModels = true; env.allowRemoteModels = false;
const ex = await pipeline('feature-extraction', 'Xenova/multilingual-e5-small', { dtype: 'q8' });
const fromStdin = repo === '--stdin';
const texts = [];
if (fromStdin) {
  texts.push(...JSON.parse(readFileSync(0, 'utf8')));
} else {
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
}
const rows = [];
const lat = [];
// One text per call, strictly in order: the q8 model uses one dynamic activation scale per batch,
// so batching would shift the vectors. A sequential reduce over promises, as migrate.ts does.
await texts.reduce(async (prev, text) => {
  await prev;
  const enc = ex.tokenizer([text], { padding: true, truncation: true });
  const ids = Array.from(enc.input_ids.data, (x) => Number(x));
  const t0 = performance.now();
  const o = await ex([text], { pooling: 'mean', normalize: true });
  lat.push(performance.now() - t0);
  rows.push({ text, ids, vec: o.tolist()[0] });
}, Promise.resolve());
if (out === '-') process.stdout.write(JSON.stringify(rows));
else writeFileSync(out, JSON.stringify(rows));
lat.sort((a, b) => a - b);
(out === '-' ? console.error : console.log)(`texts=${rows.length} p50=${lat[Math.floor(lat.length / 2)].toFixed(1)}ms p95=${lat[Math.floor(lat.length * 0.95)].toFixed(1)}ms rss=${(process.memoryUsage().rss / 1e6).toFixed(0)}MB maxTokens=${Math.max(...rows.map((r) => r.ids.length))}`);
