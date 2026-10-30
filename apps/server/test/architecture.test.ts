import { cruise } from 'dependency-cruiser';
import extractDepcruiseConfig from 'dependency-cruiser/config-utl/extract-depcruise-config';
import extractWebpackResolveConfig from 'dependency-cruiser/config-utl/extract-webpack-resolve-config';
import { describe, expect, it } from 'vite-plus/test';
import { foreignSchemaRefs } from './architecture/schema-ownership.ts';

const ruleSet = await extractDepcruiseConfig('./.dependency-cruiser.cjs');
// The CLI reads `options.webpackConfig` itself; through the API the resolve options are passed
// explicitly, so the web app's `@/` alias resolves.
const { alias } = await extractWebpackResolveConfig('./.dependency-cruiser.resolve.cjs');

const FIXTURES = 'apps/server/test/architecture/fixtures';

async function violationsIn(dir: string) {
  const result = await cruise(
    [dir],
    { ruleSet, validate: true, tsPreCompilationDeps: true },
    { alias },
  );
  if (typeof result.output === 'string') throw new Error('unexpected string output');
  return result.output.summary.violations;
}

/** Violations of one rule in a fixture directory (rules deliberately overlap; assert on one). */
async function ruleViolations(fixture: string, rule: string) {
  return (await violationsIn(`${FIXTURES}/${fixture}`)).filter((v) => v.rule.name === rule);
}

/**
 * Every dependency-cruiser rule must have a fixture that breaks it: a rule without a failing
 * fixture proves nothing. `from`/`to` are path fragments of the violating edge.
 */
const BREAKS: {
  rule: string;
  title: string;
  fixture: string;
  from: string;
  to: string;
}[] = [
  {
    rule: 'domain-is-pure',
    title: 'domain importing an npm package',
    fixture: 'domain-npm',
    from: 'modules/x/domain/a.ts',
    to: 'zod',
  },
  {
    rule: 'domain-is-pure',
    title: 'domain importing a node: builtin',
    fixture: 'domain-crypto',
    from: 'modules/x/domain/a.ts',
    to: 'crypto',
  },
  {
    rule: 'domain-is-pure',
    title: 'domain importing infrastructure',
    fixture: 'bad-domain',
    from: 'modules/x/domain/a.ts',
    to: 'modules/x/infrastructure/b.ts',
  },
  {
    rule: 'application-only-domain',
    title: 'application importing an npm package other than zod',
    fixture: 'application-npm',
    from: 'modules/x/application/a.ts',
    to: 'hono',
  },
  {
    rule: 'application-only-domain',
    title: 'application importing infrastructure',
    fixture: 'application-infra',
    from: 'modules/x/application/a.ts',
    to: 'modules/x/infrastructure/b.ts',
  },
  {
    rule: 'no-inward-infra',
    title: 'domain importing infrastructure',
    fixture: 'bad-domain',
    from: 'modules/x/domain/a.ts',
    to: 'modules/x/infrastructure/b.ts',
  },
  {
    rule: 'no-inward-infra',
    title: 'application importing infrastructure',
    fixture: 'application-infra',
    from: 'modules/x/application/a.ts',
    to: 'modules/x/infrastructure/b.ts',
  },
  {
    rule: 'modules-via-index',
    title: "a module importing another module's internals",
    fixture: 'cross-module',
    from: 'modules/x/application/a.ts',
    to: 'modules/y/domain/b.ts',
  },
  {
    rule: 'no-radix',
    title: 'importing Radix',
    fixture: 'radix',
    from: 'fixtures/radix/a.ts',
    to: 'radix-ui',
  },
  {
    rule: 'web-no-server',
    title: 'the web SPA importing the server',
    fixture: 'web-server',
    from: 'apps/web/src/a.ts',
    to: 'apps/server/b.ts',
  },
  {
    rule: 'api-no-ui',
    title: 'the web api client importing UI components',
    fixture: 'api-ui',
    from: 'apps/web/src/api/a.ts',
    to: 'apps/web/src/components/b.ts',
  },
  {
    rule: 'no-circular',
    title: 'a dependency cycle',
    fixture: 'circular',
    from: 'circular/a.ts',
    to: 'circular/b.ts',
  },
  {
    rule: 'npm-only-in-infra',
    title: 'postgres in application',
    fixture: 'npm-postgres-in-application',
    from: 'modules/x/application/a.ts',
    to: 'postgres',
  },
  {
    rule: 'npm-only-in-infra',
    title: 'openid-client in application',
    fixture: 'npm-openid-in-application',
    from: 'modules/x/application/a.ts',
    to: 'openid-client',
  },
  {
    rule: 'npm-only-in-infra',
    title: 'the MCP SDK in application',
    fixture: 'npm-mcp-in-application',
    from: 'modules/x/application/a.ts',
    to: '@modelcontextprotocol/server',
  },
  {
    rule: 'npm-only-in-infra',
    title: 'node:crypto in shared-kernel',
    fixture: 'npm-crypto-in-shared-kernel',
    from: 'shared-kernel/a.ts',
    to: 'crypto',
  },
  {
    rule: 'npm-only-in-infra',
    title: 'postgres in the web SPA',
    fixture: 'npm-postgres-in-web',
    from: 'apps/web/src/a.ts',
    to: 'postgres',
  },
  {
    rule: 'npm-only-in-infra-transport',
    title: 'postgres in interfaces',
    fixture: 'npm-postgres-in-interfaces',
    from: 'modules/x/interfaces/a.ts',
    to: 'postgres',
  },
  {
    rule: 'npm-only-in-infra-transport',
    title: 'node:crypto in interfaces',
    fixture: 'npm-crypto-in-interfaces',
    from: 'modules/x/interfaces/a.ts',
    to: 'crypto',
  },
  {
    rule: 'npm-only-in-infra-transport',
    title: 'openid-client in interfaces',
    fixture: 'npm-openid-in-interfaces',
    from: 'modules/x/interfaces/a.ts',
    to: 'openid-client',
  },
  {
    rule: 'npm-only-in-infra',
    title: 'the SDK validator ajv in application',
    fixture: 'npm-ajv-in-domain',
    from: 'modules/x/application/a.ts',
    to: 'ajv',
  },
];

describe('architecture rules', () => {
  it.each(BREAKS)('$rule rejects $title', async ({ rule, fixture, from, to }) => {
    const found = await ruleViolations(fixture, rule);
    expect(found).toHaveLength(1);
    expect(found[0]).toMatchObject({
      from: expect.stringContaining(from),
      to: expect.stringContaining(to),
    });
  });

  it('every rule of .dependency-cruiser.cjs has at least one failing fixture', () => {
    const covered = new Set(BREAKS.map((b) => b.rule));
    const rules = (ruleSet.forbidden ?? []).flatMap((r) => (r.name === undefined ? [] : [r.name]));
    expect(rules.filter((name) => !covered.has(name))).toEqual([]);
  });

  it('npm-only-in-infra lets infrastructure, platform, transport-in-interfaces and zod-in-application through', async () => {
    expect(await violationsIn(`${FIXTURES}/npm-allowed`)).toEqual([]);
  });

  it('accepts the real source tree', async () => {
    expect(await violationsIn('apps/server/src')).toEqual([]);
  });

  it('accepts the real web source tree', async () => {
    expect(await violationsIn('apps/web/src')).toEqual([]);
  });
});

describe('schema ownership: a module reads and writes only its own schema', () => {
  it("rejects SQL naming another module's schema, in a template or a plain string", async () => {
    const found = await foreignSchemaRefs(`${FIXTURES}/schema-foreign/modules`);
    const rows = found.map((v) => [v.file.split('/modules/')[1] ?? '', v.module, v.ref]);
    expect(rows.toSorted((a, b) => (a[0] ?? '').localeCompare(b[0] ?? ''))).toEqual([
      ['connections/infrastructure/a.ts', 'connections', 'catalog.servers'],
      ['connections/infrastructure/b.ts', 'connections', 'identity.api_tokens'],
    ]);
  });

  it('lets its own schema, platform.*, event names, non-SQL text and tests through', async () => {
    expect(await foreignSchemaRefs(`${FIXTURES}/schema-allowed/modules`)).toEqual([]);
  });

  it('accepts the real modules', async () => {
    expect(await foreignSchemaRefs('apps/server/src/modules')).toEqual([]);
  });
});
