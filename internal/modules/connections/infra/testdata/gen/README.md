`gen.mjs` regenerates `../formencode.json` and `../basic.json` with node's `URLSearchParams` (the reference's form encoding of Basic credentials): `cd internal/modules/connections/infra/testdata && node gen/gen.mjs`.
The committed tables are formatted by the repository formatter; the content (not the whitespace) is what the generator reproduces.
