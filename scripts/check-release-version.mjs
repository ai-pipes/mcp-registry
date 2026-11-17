// Release guard: `node scripts/check-release-version.mjs <tag>` fails unless the tag is `v` plus
// the version in apps/server/package.json (the only source of the gateway version).
// Exit codes: 0 match (prints the version), 1 mismatch, 2 no tag given.
import { readFileSync } from 'node:fs';

const tag = process.argv[2];
if (tag === undefined || tag === '') {
  console.error('usage: node scripts/check-release-version.mjs <tag>');
  process.exit(2);
}

const pkg = JSON.parse(
  readFileSync(new URL('../apps/server/package.json', import.meta.url), 'utf8'),
);
const version = typeof pkg.version === 'string' ? pkg.version : '';
if (tag !== `v${version}`) {
  console.error(`tag ${tag} does not match apps/server/package.json version ${version}`);
  process.exit(1);
}
console.log(version);
