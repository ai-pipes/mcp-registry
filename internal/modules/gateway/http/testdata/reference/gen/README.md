`cases.json` is the table of requests the differential test of `/mcp` (`TestTheReferenceAnswersTheseRequests`) replays; `answers.json` is what the reference's own handler answers to each.

- `python3 gen/cases.py` writes `../cases.json`.
- `node gen/answers.mjs` writes `../answers.json` by running `createMcpHandler` of `@modelcontextprotocol/server` (the version in the reference's `pnpm-lock.yaml`, 2.4.0) in-process, with the four meta-tools under their real names, descriptions and schemas. It needs `MCP_REFERENCE_ROOT=<checkout>`: an installed source tree of the tag `v1.0.1` (below).

Both outputs are deterministic and indented by two spaces, so a run leaves `git diff --exit-code .` clean.

**Provenance: v1.0.1.** The committed `answers.json` was last regenerated from the reference 1.0.1 (the TypeScript server of this tree before 2.0.0, unchanged since the tag `v1.0.1`, and again from an unpacked `git archive v1.0.1`); both runs reproduced the committed file byte for byte. Regenerate from the source of the tag:

```sh
REF=$(mktemp -d)
git archive v1.0.1 | tar -x -C "$REF"
(cd "$REF" && pnpm install --frozen-lockfile --ignore-scripts)
MCP_REFERENCE_ROOT="$REF" node internal/modules/gateway/http/testdata/reference/gen/answers.mjs
```
