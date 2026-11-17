import { bootstrapLocalAdmin, loginLinkUrl } from '../modules/identity/index.ts';
import type { Config } from './config.ts';
import type { Container } from './container.ts';
import type { Logger } from './logger.ts';

/**
 * First start of a server without OIDC: `bootstrapLocalAdmin` decides whether a sign-in link is
 * due; this only logs it. The one-time code appears in this single record (field `url`), never
 * in audit records or errors. With OIDC configured nobody is created and nothing is printed.
 */
export async function announceFirstStart(
  container: Container,
  config: Config,
  logger: Logger,
): Promise<void> {
  if (config.oidc.client !== null) return;
  const link = await bootstrapLocalAdmin(container.identity);
  if (link === null) return;
  logger.warn(
    { url: loginLinkUrl(config.publicUrl, link.code) },
    'first start: open this one-time link within 15 minutes to sign in as admin',
  );
}
