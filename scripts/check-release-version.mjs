// Release guard: `node scripts/check-release-version.mjs <tag>` fails unless the tag is `v` plus
// the version in the root VERSION file (the only source of the gateway version), and
// CHANGELOG.md must have a dated heading `## [<version>] - YYYY-MM-DD` for it (so
// `## [1.0.0] - Unreleased` blocks the release until it is dated).
// `--rehearsal` (every non-tag workflow run: manual or pull request) only requires a non-empty version, prints it and skips the tag
// and changelog checks.
// Exit codes: 0 match (prints the version), 1 mismatch, empty version or no dated changelog entry,
// 2 no tag given.
import { readFileSync } from 'node:fs';

const tag = process.argv[2];
if (tag === undefined || tag === '') {
  console.error('usage: node scripts/check-release-version.mjs <tag> | --rehearsal');
  process.exit(2);
}

const version = readFileSync(new URL('../VERSION', import.meta.url), 'utf8').trim();
if (tag === '--rehearsal') {
  if (version === '') {
    console.error('VERSION has no version');
    process.exit(1);
  }
  console.log(version);
  process.exit(0);
}
if (tag !== `v${version}`) {
  console.error(`tag ${tag} does not match the VERSION file ${version}`);
  process.exit(1);
}
const changelog = readFileSync(new URL('../CHANGELOG.md', import.meta.url), 'utf8');
const escaped = version.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
const heading = new RegExp(`^## \\[${escaped}\\] - \\d{4}-\\d{2}-\\d{2}\\s*$`, 'm');
if (!heading.test(changelog)) {
  console.error(`CHANGELOG.md has no dated entry for ${version}`);
  process.exit(1);
}
console.log(version);
