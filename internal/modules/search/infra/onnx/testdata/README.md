# Test data of the local embedder

| File                                 | What                                                                                                                                                                                                                                                                                                                                                                                                     | CPU dependent |
| ------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------- |
| `reference.json`                     | 1287 texts (292 curated: the compat suite's passages and queries plus adversarial strings; 36 targeted Unicode strings; a seeded fuzz sample of 959 out of 5751), each with the output of the JavaScript normaliser (`Precompiled.normalize`) and the token ids of the reference tokenizer. 4 curated texts with lone surrogates (296 in the full curated set) are left out: they cannot be a Go string. | no            |
| `pooling.json`                       | fixed float32 hidden states (5 lengths x 384) with the vector transformers.js makes of them (`mean_pooling`, `Tensor.normalize`); base64 little-endian float32.                                                                                                                                                                                                                                          | no            |
| `gen/reference.mjs`, `gen/texts.mjs` | generators of the two files above. They run in the reference image (`mcp-registry:compat-ts`, Node 22.23.5, Unicode 17.0, transformers 4.3.2, tokenizers 0.2.0); the command is in the script header.                                                                                                                                                                                                    |               |
| `gen/vectors.mjs`                    | makes the reference VECTORS of the texts of `reference.json` on the machine or image that will run the Go comparison. Not committed (vectors depend on the CPU). Used with `ONNX_TEST_REFERENCE_VECTORS`.                                                                                                                                                                                                | yes           |

Regenerate `reference.json` and `pooling.json`:

```sh
docker run --rm --network none -v "$PWD":/repo:ro -v "$OUT":/out mcp-registry:compat-ts \
  node /repo/internal/modules/search/infra/onnx/testdata/gen/reference.mjs \
  /app/apps/server/node_modules/@huggingface/transformers/dist/transformers.node.mjs \
  /app/models /repo/compat/fixtures/tools /out
```

then copy `$OUT/*.json` here and format them with `pnpm exec vp fmt` (the generator writes compact
JSON; the committed files are formatted). Provenance: v1.0.1. The last run in the reference 1.0.1
with the fixtures of `compat/fixtures/tools` reproduced both committed files byte for byte after
formatting. The normaliser pairs depend on the Unicode version of the Node that
made them (17.0 here); Go's `golang.org/x/text` has its own tables, and the test compares them.
