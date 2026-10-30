import { err, ok, type Result } from '../../../shared-kernel/index.ts';
import { CatalogError } from './errors.ts';

const SLUG = /^[a-z][a-z0-9-]{1,31}$/;

/** A server's slug: kebab-case, 2-32 characters, the prefix of its tool names. */
export const ServerSlug = {
  parse(raw: string): Result<string, CatalogError> {
    if (!SLUG.test(raw)) {
      return err(
        new CatalogError(
          'invalid_slug',
          'server slug must be 2-32 characters: lowercase letters, digits and dashes, starting with a letter',
        ),
      );
    }
    return ok(raw);
  },
};
