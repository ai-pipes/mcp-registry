import { cruise } from 'dependency-cruiser';
import extractDepcruiseConfig from 'dependency-cruiser/config-utl/extract-depcruise-config';
import extractWebpackResolveConfig from 'dependency-cruiser/config-utl/extract-webpack-resolve-config';
import { describe, expect, it } from 'vite-plus/test';

// The rules of .dependency-cruiser.cjs (`pnpm arch`) cover the TypeScript that remains: the web UI
// and the REST contracts. The server's architecture rules are Go's (.go-arch-lint.yml, depguard,
// internal/archtest). Run from the repository root (the `arch` test project).
const ruleSet = await extractDepcruiseConfig('./.dependency-cruiser.cjs');
// The CLI reads `options.webpackConfig` itself; through the API the resolve options are passed
// explicitly, so the web app's `@/` alias resolves.
const { alias } = await extractWebpackResolveConfig('./.dependency-cruiser.resolve.cjs');

const FIXTURES = 'apps/web/test/architecture/fixtures';

async function violationsIn(dir: string) {
  const result = await cruise(
    [dir],
    { ruleSet, validate: true, tsPreCompilationDeps: true },
    { alias },
  );
  if (typeof result.output === 'string') throw new Error('unexpected string output');
  return result.output.summary.violations;
}

/** Violations of one rule in a fixture directory. */
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
    rule: 'no-radix',
    title: 'importing Radix',
    fixture: 'radix',
    from: 'fixtures/radix/a.ts',
    to: 'radix-ui',
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

  it('accepts the real web source tree', async () => {
    expect(await violationsIn('apps/web/src')).toEqual([]);
  });

  it('accepts the real contracts source tree', async () => {
    expect(await violationsIn('packages/contracts/src')).toEqual([]);
  });
});
