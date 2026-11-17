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

## Releasing

1. Rehearse: `gh workflow run release --ref <branch>` (once the workflow file with the `workflow_dispatch` trigger is on `main`) and wait for a green result. A pull request that touches the release path (workflow, Dockerfile, compose files, smoke scripts, lockfile, the version in `apps/server/package.json`, `CHANGELOG.md`) rehearses automatically. A rehearsal builds and tests everything (both architectures, both smoke tests) but never publishes and needs no secrets.
2. Date the changelog entry (`## [<version>] - YYYY-MM-DD`; `Unreleased` blocks the release) and add the compare link at the bottom.
3. Make sure `apps/server/package.json` has the version (the only place it is set).
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
