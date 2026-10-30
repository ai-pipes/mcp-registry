const TOKEN_MARK = 'sk_';
const PREFIX_LENGTH = 10;
const MAX_TOKEN_LENGTH = 100;
const LAST_USED_GRANULARITY_MS = 60_000;

/** Cheap shape check before a token reaches the database. */
export function looksLikeToken(token: string): boolean {
  return token.length <= MAX_TOKEN_LENGTH && token.startsWith(TOKEN_MARK);
}

export function newTokenValue(randomPart: string): string {
  return `${TOKEN_MARK}${randomPart}`;
}

/** The part of a token that is safe to show in lists. */
export function tokenPrefix(token: string): string {
  return token.slice(0, PREFIX_LENGTH);
}

/** `lastUsedAt` is refreshed at most once a minute to keep reads from becoming writes. */
export function shouldTouchLastUsed(lastUsedAt: Date | null, now: Date): boolean {
  return !lastUsedAt || now.getTime() - lastUsedAt.getTime() >= LAST_USED_GRANULARITY_MS;
}
