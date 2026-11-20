# Contributing

Thanks for helping. Bug reports, fixes and small focused features are welcome. For larger changes, open an issue first to agree on the approach.

## Setup

You need Node 22, pnpm (the version pinned in `package.json`; `corepack enable` picks it up) and Docker (integration tests start Postgres through Testcontainers).

```sh
pnpm install          # also installs lefthook git hooks
pnpm verify           # must be green before you open a PR
```

Optional, for changes that affect the image or the UI end to end:

```sh
bash scripts/smoke.sh        # builds the image, runs CLI -> MCP search -> describe -> call
bash scripts/e2e-image.sh    # Playwright against the built image (pnpm exec playwright install chromium)
```

## How we work

- **`pnpm verify` must pass**: format, lint, types, architecture rules, knip, unit and integration tests, UI build. The pre-push hook runs it; do not skip hooks with `--no-verify`.
- **Tests first.** Write the failing test, then the code. Domain rules get a unit test next to the code; use cases are tested with fake ports; adapters get an integration test against real Postgres (`apps/server/test/**/*.int.test.ts`). Coverage of `domain` and `application` must stay at or above 90%.
- **Respect the architecture.** The server is a modular monolith with DDD layers; import rules are enforced by dependency-cruiser and by architecture tests with failing fixtures, and each module owns its database schema. Read [CLAUDE.md](./CLAUDE.md) before your first change: it lists the layers, the rules, the domain vocabulary and the recipe for a new use case.
- **Never weaken a rule to get green.** No `eslint-disable`, no `@ts-ignore`, no edits to `.dependency-cruiser.cjs` or coverage thresholds, no `it.skip`, no CSP exceptions. Fix the code. If you believe a rule is wrong, propose the change in a separate PR with its reasoning.
- **UI** uses shadcn/ui on Base UI (no Radix); all strings live in `apps/web/src/strings.ts`; the CSP forbids inline scripts and `<style>` elements.
- **Never log secrets, tokens, or call arguments and results.**
- **Changelog.** A change an operator can see (a setting, a CLI command, an upgrade step, a changed default, a fix they would notice) adds a line to `CHANGELOG.md` in the same pull request.
- Keep docs in step: a new module, term, command or environment variable updates `CLAUDE.md`, `README.md` and `.env.example` in the same PR.

## Go rewrite and the compatibility suite

The Go server is being built beside the TypeScript one; `compat/` is the Go black-box suite both must satisfy (details in [CLAUDE.md](./CLAUDE.md), "Go rewrite").

```sh
make go-verify              # Go lint, architecture rules, vet, unit tests (CI job `go`)
make compat-image-ts        # builds mcp-registry:compat-ts from this tree
make compat                 # the suite against that image (needs Docker, 9-11 minutes; CI job `compat`)
make compat-selfcheck       # proves the suite fails on a deliberately broken image (daily in CI)
make e2e-flows              # the web UI's browser flows (Playwright, Chromium) against the same image (CI job `e2e-flows`)
```

`make e2e-flows` needs `pnpm install` and `pnpm exec playwright install chromium`. The flows (`e2e/flows/*.spec.ts`) cannot run on their own: the Go test `TestBrowserFlows` (`compat/e2e`) boots the stacks and the fakes and passes a control API (127.0.0.1, per-run bearer token, fixed operations only) to them. A failed run leaves traces and screenshots in `node_modules/.cache/playwright/flows-results` (`pnpm exec playwright show-trace <dir>/trace.zip`). A new flow goes in `e2e/flows/NN-name.spec.ts`, takes texts from `apps/web/src/strings.ts`, uses role-based locators, never waits for a fixed time, and is shown to fail when its expectation is broken.

- **What the image under test must be.** The suite runs an IMAGE: it starts the server with the image's default command (so the image declares `CMD`, not `ENTRYPOINT`: the harness runs the operator CLI in the same image by replacing the command with `COMPAT_CLI` plus the arguments), listening on port 3000 inside the container (the harness publishes it; `PORT` is set only by the one scenario that moves it), configured through the environment variables of `apps/server/src/platform/config.ts`. The database is `pgvector/pgvector:pg17`, started by the harness. To judge a locally built Go binary, wrap it in an image: `FROM gcr.io/distroless/base-debian12` (or any base with the libraries it needs), `COPY mcp-registry /usr/local/bin/mcp-registry`, `CMD ["mcp-registry", "serve"]` (whatever starts the server), and run `COMPAT_IMAGE=<tag> COMPAT_CLI='mcp-registry' make compat`.
- Run against another image with `COMPAT_IMAGE` (default `mcp-registry:compat-ts`) and `COMPAT_CLI` (how the CLI is invoked inside it, default `node dist/cli.js`). To keep the app log of failed tests, set `COMPAT_LOG_DIR=<dir>`; `COMPAT_APP_ENV=NAME=value,...` adds non-secret variables to the app container.
- Other variables: `COMPAT_UPGRADE_FROM` (the image that creates the state in the upgrade scenario, default `COMPAT_IMAGE`; set it to the TypeScript 1.x image when `COMPAT_IMAGE` is the Go one) with `COMPAT_UPGRADE_FROM_CLI` (its CLI invocation), `COMPAT_REFERENCE_IMAGE` (see goldens below), `SELFCHECK_BASE` (the image `make compat-selfcheck` derives its broken image from, default `mcp-registry:compat-ts`), `E2E_PLAYWRIGHT_ARGS` (selects flows for `make e2e-flows`, see CLAUDE.md for what is refused) and `COMPAT_ARGS` (extra `go test` arguments for `make compat`).
- Run a subset: `make compat COMPAT_ARGS='-run ^TestPlatform'`. The `-run` patterns per area:

  | Area                                                                       | `-run` pattern                                             |
  | -------------------------------------------------------------------------- | ---------------------------------------------------------- |
  | platform (headers, CSRF, static files, migrations, key check, first start) | `^(TestPlatform\|TestFirstStart)`                          |
  | identity (OIDC, link sign-in, sessions, rate limits)                       | `^TestIdentity`                                            |
  | catalog (servers, sync, concurrency, validation)                           | `^TestCatalog(Servers\|OptimisticConcurrency\|Validation)` |
  | search (index, recorded queries, filters, stored vectors)                  | `^(TestCatalogSearch\|TestSearch)`                         |
  | connections (OAuth, PAT, refresh)                                          | `^TestConnections`                                         |
  | MCP (protocol, surface, meta-tools)                                        | `^TestMCP`                                                 |
  | gateway (call_tool, faults, REST calls and audit)                          | `^TestGateway`                                             |
  | CLI                                                                        | `^TestCLI`                                                 |
  | upgrade                                                                    | `^TestUpgrade`                                             |
  | config (refused settings, `*_FILE`, ports, hosts, embedder)                | `^TestConfig`                                              |

- **A scenario changes only together with confirmation against the 1.0.0 TypeScript image.** Never edit a scenario or a golden to make the Go server pass; if the Go server differs, the Go server is wrong unless the owner decides otherwise.
- Goldens are rewritten only against the TypeScript reference image: `COMPAT_REFERENCE=1 go test -tags compat ./compat/ -run '<the test that owns it>' -update`. `-update` is refused unless `COMPAT_REFERENCE=1` AND the image under test is the reference image: the built-in default tag, or exactly the value of `COMPAT_REFERENCE_IMAGE` when that is set (CI can name its reference image this way). `make compat` sets neither, so `COMPAT_IMAGE=<go image> make compat-record-search` is refused. `-update` rewrites every golden of the tests it runs, so restrict it with `-run`.
- The search golden is per architecture (`compat/golden/search-arm64.json`, `search-amd64.json`). `make compat-record-search` records the one for your machine; for the other architecture run the `compat-record` workflow (`gh workflow run compat-record`), download its artifact and commit the file. Until `search-amd64.json` is committed, the amd64 entry of the CI job `compat` records it in the run (`make compat-record-search`), uploads it as the artifact `search-amd64`, warns with `::warning::` that it must be committed, and then runs the whole suite, so everything else is verified on amd64 either way. The stored vectors (`TestSearchStoredVectors`) are compared with `compat/golden/embeddings-<arch>.json`, which `compat/golden/gen-embeddings.mjs` records from `@huggingface/transformers` directly; it is committed for both architectures and never rewritten by `-update`.
- A new check in the suite should have a matching distortion in `compat/selfcheck` when it guards a property a broken server could plausibly lose.

## Releasing

1. Rehearse: `gh workflow run release --ref <branch>` (once the workflow file with the `workflow_dispatch` trigger is on `main`) and wait for a green result. A pull request that touches the release path (workflow, Dockerfile, compose files, smoke scripts, lockfile, the version in `apps/server/package.json`, `CHANGELOG.md`) rehearses automatically. A rehearsal builds and tests everything (both architectures, both smoke tests) but never publishes and needs no secrets.
2. Date the changelog entry (`## [<version>] - YYYY-MM-DD`; `Unreleased` blocks the release) and add the compare link at the bottom.
3. Make sure `apps/server/package.json` has the version (the only place it is set), and update the two places that pin the release tag: the quick-start command in `README.md` and the usage line in the header of `quickstart.compose.yml`.
4. Before tagging, confirm the rehearsal on the release-prep pull request is green and shows no warning from the strict check ("a tag ... pushed now would be refused").
5. Merge to `main`.
6. Tag `v<version>` on `main` and push the tag. The release workflow checks the tag, the changelog entry and that the commit is on `main`, builds the image, tests it (version printed by the amd64 and arm64 images, quick-start smoke, compose smoke) and only then pushes it to GHCR.
7. After the workflow finishes, confirm an anonymous pull works (`docker logout ghcr.io; docker pull ghcr.io/ai-pipes/mcp-registry:<version>`) and that the package is public and linked to the repository.

## Commits

[Conventional Commits](https://www.conventionalcommits.org/), as in the git log:

```
feat(mcp): own body limit for /mcp, MCP_MAX_BODY_BYTES (default 10 MiB)
fix(http): browser callbacks redirect with 'internal' on unexpected errors
refactor(catalog): sync owner rule into catalog
docs: ...
```

Types: `feat`, `fix`, `refactor`, `test`, `docs`, `chore`, `ci`, `build`, `perf`. The scope is the module or area (`identity`, `catalog`, `connections`, `search`, `gateway`, `mcp`, `http`, `web`, `cli`...). Use the imperative mood and keep the subject short.

## Pull request checklist

- [ ] `pnpm verify` is green locally.
- [ ] New behaviour is covered by tests written first; bug fixes include a regression test.
- [ ] No architecture, lint or coverage rule was weakened.
- [ ] No secrets, tokens or call payloads are logged or returned to clients.
- [ ] `CLAUDE.md`, `README.md` and `.env.example` are updated if behaviour, configuration or vocabulary changed.
- [ ] Database changes come as a new migration in `apps/server/migrations` (never edit an applied one) and stay inside the module's own schema.

By contributing you agree that your contributions are licensed under the [MIT License](./LICENSE).
