// Vectors of the reference for the texts of reference.json, made exactly as the TypeScript server
// makes them (feature-extraction, dtype q8, mean pooling, normalize, one text per call). The output
// is [{i, vec}] with i the index in reference.json; it is NOT committed: vectors depend on the CPU
// and the OS build of ONNX Runtime, so they are generated on the machine (or in the image) that
// runs the Go comparison (ONNX_TEST_REFERENCE_VECTORS, test TestVectorsMatchTheReferenceBitForBit).
//   node vectors.mjs <transformers entry> <model root> <reference.json> <out.json>
import { readFileSync, writeFileSync } from 'node:fs';

const [, , transformersPath, modelDir, referencePath, outPath] = process.argv;
const { env, pipeline } = await import(transformersPath);
env.localModelPath = modelDir;
env.cacheDir = modelDir;
env.allowLocalModels = true;
env.allowRemoteModels = false;
const extractor = await pipeline('feature-extraction', 'Xenova/multilingual-e5-small', {
  dtype: 'q8',
});
const { cases } = JSON.parse(readFileSync(referencePath, 'utf8'));
const rows = [];
// One text per call, one after the other (a batch would change the vectors).
await cases.reduce(
  (previous, { text }, i) =>
    previous.then(async () => {
      const out = await extractor([text], { pooling: 'mean', normalize: true });
      rows.push({ i, vec: Array.from(out.data) });
    }),
  Promise.resolve(),
);
writeFileSync(outPath, JSON.stringify(rows));
console.log(`${rows.length} vectors`);
