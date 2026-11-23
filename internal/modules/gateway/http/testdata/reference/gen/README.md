`cases.json` is the table of requests the differential test of `/mcp` (`TestTheReferenceAnswersTheseRequests`) replays; `answers.json` is what the reference's own handler answers to each.

- `python3 gen/cases.py` writes `../cases.json`.
- `node gen/answers.mjs` writes `../answers.json` by running `createMcpHandler` of `@modelcontextprotocol/server` (the version in `pnpm-lock.yaml`, 2.4.0) in-process, with the four meta-tools under their real names, descriptions and schemas. It needs the repository's `pnpm install`; `MCP_REFERENCE_ROOT=<checkout>` takes the packages of another checkout.

Both outputs are deterministic and indented by two spaces, so a run leaves `git diff --exit-code .` clean.
