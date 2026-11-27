/**
 * Architecture rules of the TypeScript that remains (the web UI in apps/web and the REST contracts in
 * packages/contracts; `pnpm arch`). The server is Go: its rules are .go-arch-lint.yml, depguard in
 * .golangci.yml and internal/archtest. Every rule here has a failing fixture in
 * apps/web/test/architecture/fixtures (apps/web/test/architecture.test.ts).
 */

/** @type {import('dependency-cruiser').IConfiguration} */
module.exports = {
  forbidden: [
    {
      name: 'no-radix',
      comment:
        'the UI kit is shadcn/ui on Base UI; Radix packages (`radix-ui`, `@radix-ui/*`) are not used anywhere.',
      severity: 'error',
      from: {},
      to: { path: '(?:^|/)(?:radix-ui|@radix-ui/[^/]+)(?:/|$)' },
    },
    {
      name: 'api-no-ui',
      comment: 'the web API client (src/api) does not depend on UI components.',
      severity: 'error',
      from: { path: '(?:^|/)apps/web/src/api/' },
      to: { path: '(?:^|/)apps/web/src/components/' },
    },
    {
      name: 'web-only-web-and-contracts',
      comment:
        'the web app (apps/web/src) imports only itself, the contracts package (packages/contracts, ' +
        'resolved through the workspace symlink to its real path) and npm packages: not e2e/, ' +
        'scripts/, compat/, the root config or anything else in the repository. Paths are relative ' +
        'to the repository root.',
      severity: 'error',
      from: { path: '^apps/web/src/' },
      to: { pathNot: '^(?:apps/web|packages/contracts)/|(?:^|/)node_modules/' },
    },
    {
      name: 'no-circular',
      comment: 'circular dependencies are forbidden.',
      severity: 'error',
      from: {},
      to: { circular: true },
    },
  ],
  options: {
    doNotFollow: { path: 'node_modules' },
    // @swc/core (a root devDependency; knip.json ignores it, nothing imports it) parses the sources:
    // dependency-cruiser cannot use TypeScript 7's compiler yet.
    parser: 'swc',
    tsPreCompilationDeps: true,
    // Resolves the web app's `@/` alias (apps/web/src).
    webpackConfig: { fileName: '.dependency-cruiser.resolve.cjs' },
    enhancedResolveOptions: {
      exportsFields: ['exports'],
      conditionNames: ['import', 'require', 'node', 'default', 'types'],
      extensions: ['.ts', '.tsx', '.js', '.mjs', '.cjs', '.json'],
    },
  },
};
