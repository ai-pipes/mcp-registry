import { readFileSync } from 'node:fs';

// Set by the build (`define` in the root vite.config.ts) from apps/server/package.json, the only
// source of the version. The built package does not ship package.json.
declare const GATEWAY_BUILD_VERSION: string | undefined;

function readPackageVersion(): string {
  const pkg: unknown = JSON.parse(
    readFileSync(new URL('../../package.json', import.meta.url), 'utf8'),
  );
  if (
    typeof pkg === 'object' &&
    pkg !== null &&
    'version' in pkg &&
    typeof pkg.version === 'string'
  ) {
    return pkg.version;
  }
  throw new Error('apps/server/package.json has no version');
}

/** Gateway version: the build-time value, or (running from sources) the package.json field. */
export const VERSION: string =
  typeof GATEWAY_BUILD_VERSION === 'string' && GATEWAY_BUILD_VERSION !== ''
    ? GATEWAY_BUILD_VERSION
    : readPackageVersion();
