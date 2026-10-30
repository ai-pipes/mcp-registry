const LOGIN_STATE_TTL_MS = 10 * 60_000;

/** When a login attempt started at `now` stops being valid. */
export function loginStateExpiry(now: Date): Date {
  return new Date(now.getTime() + LOGIN_STATE_TTL_MS);
}

export function isLoginStateExpired(expiresAt: Date, now: Date): boolean {
  return now.getTime() >= expiresAt.getTime();
}

/** Only a same-origin relative path may be a post-login target; anything else becomes `/`. */
export function normalizeReturnTo(raw: string): string {
  if (!raw.startsWith('/') || raw.startsWith('//') || raw.startsWith('/\\')) return '/';
  // CR/LF and other control characters would split headers
  for (const ch of raw) {
    if (ch.charCodeAt(0) < 0x20 || ch === '\u007f') return '/';
  }
  return raw;
}
