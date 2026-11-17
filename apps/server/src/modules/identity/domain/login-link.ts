const LOGIN_LINK_TTL_MS = 15 * 60_000;
const CODE_MARK = 'll_';
const MAX_CODE_LENGTH = 100;

/** A login link is valid for 15 minutes after it is issued. */
export function loginLinkExpiry(now: Date): Date {
  return new Date(now.getTime() + LOGIN_LINK_TTL_MS);
}

export function isLoginLinkExpired(expiresAt: Date, now: Date): boolean {
  return now.getTime() >= expiresAt.getTime();
}

export function newLoginCode(randomPart: string): string {
  return `${CODE_MARK}${randomPart}`;
}

/** Cheap shape check before a code reaches the database. */
export function looksLikeLoginCode(code: string): boolean {
  return code.length <= MAX_CODE_LENGTH && code.startsWith(CODE_MARK);
}

/** The code sits in the URL fragment, which browsers do not send to the server. */
export function loginLinkUrl(publicUrl: string, code: string): string {
  return `${publicUrl.replace(/\/$/, '')}/login#link=${code}`;
}

/**
 * Whether a server without OIDC must hand out a first-admin link at start: `create` when there
 * are no people at all, `reissue` while the only person is the enabled local admin who has never
 * signed in (so a lost first-start link cannot lock the install out), else `none`.
 */
export function needsBootstrapLink(
  people: readonly { sub: string; disabled: boolean; createdAt: Date; lastLoginAt: Date | null }[],
  localAdminSub: string,
): 'create' | 'reissue' | 'none' {
  const [only] = people;
  if (!only) return 'create';
  if (people.length !== 1 || only.sub !== localAdminSub || only.disabled) return 'none';
  // Provisioning stamps lastLoginAt = createdAt; any real sign-in moves it later.
  const neverSignedIn =
    only.lastLoginAt === null || only.lastLoginAt.getTime() <= only.createdAt.getTime();
  return neverSignedIn ? 'reissue' : 'none';
}
