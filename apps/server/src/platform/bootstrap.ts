import { bootstrapLocalAdmin, loginLinkUrl } from '../modules/identity/index.ts';
import { errorFields } from '../shared-kernel/index.ts';
import { verifySecretsKey } from './crypto/key-check.ts';
import type { Sql } from './db.ts';
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
  try {
    const link = await bootstrapLocalAdmin(container.identity);
    if (link === null) return;
    logger.warn(
      { url: loginLinkUrl(config.publicUrl, link.code) },
      'first start: open this one-time link within 15 minutes to sign in as admin',
    );
  } catch (error) {
    // The server still starts; the next start tries again while local-admin has never signed in.
    logger.error({ err: errorFields(error) }, 'first start: could not issue the sign-in link');
  }
}

/**
 * Start-up check that SECRETS_KEY is the key recorded on the first start with the check (it does
 * not test the stored secrets themselves). Logs the verdict
 * (never key material) and returns whether the server may continue.
 */
export async function checkSecretsKey(
  sql: Sql,
  config: Pick<Config, 'secretsKey' | 'secretsKeyOld'>,
  logger: Logger,
): Promise<{ proceed: boolean }> {
  const verdict = await verifySecretsKey(sql, {
    current: config.secretsKey,
    ...(config.secretsKeyOld ? { previous: config.secretsKeyOld } : {}),
  });
  if (verdict === 'created') {
    logger.info('secrets key check created: SECRETS_KEY is now verified at every start');
  } else if (verdict === 'needs_rotation') {
    logger.warn(
      'the secrets key check still opens with SECRETS_KEY_OLD only: run rotate-secrets-key',
    );
  } else if (verdict === 'mismatch') {
    logger.fatal(
      'SECRETS_KEY does not match the data in this database; restore the original key (stored secrets cannot be read with this one). See "Recovering from a key mismatch" in the README.',
    );
  }
  return { proceed: verdict !== 'mismatch' };
}
