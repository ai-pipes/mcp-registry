import { describe, expect, it } from 'vite-plus/test';
import { deadSessionCutoffs, isSessionAlive, sessionExpiry } from './session.ts';

const t0 = new Date('2026-01-01T00:00:00Z');
const plus = (d: Date, ms: number) => new Date(d.getTime() + ms);
const H = 3600_000;

describe('session lifetime', () => {
  it('has a 30-day absolute limit', () => {
    expect(sessionExpiry(t0).getTime() - t0.getTime()).toBe(30 * 24 * H);
  });
  it('is alive within idle and absolute limits', () => {
    const s = { lastSeenAt: t0, expiresAt: sessionExpiry(t0) };
    expect(isSessionAlive(s, plus(t0, 12 * H))).toBe(true);
  });
  it('dies after 12 hours idle', () => {
    const s = { lastSeenAt: t0, expiresAt: sessionExpiry(t0) };
    expect(isSessionAlive(s, plus(t0, 12 * H + 1))).toBe(false);
  });
  it('dies at the absolute limit even if active', () => {
    const exp = sessionExpiry(t0);
    const s = { lastSeenAt: plus(exp, -1000), expiresAt: exp };
    expect(isSessionAlive(s, exp)).toBe(false);
  });
});

describe('deadSessionCutoffs', () => {
  it('a session past either cutoff is exactly one isSessionAlive refuses', () => {
    const now = plus(t0, 40 * 24 * H);
    const { expiresAtOrBefore, lastSeenBefore } = deadSessionCutoffs(now);
    expect(expiresAtOrBefore).toEqual(now);
    expect(lastSeenBefore).toEqual(plus(now, -12 * H));
    const dead = (s: { lastSeenAt: Date; expiresAt: Date }) =>
      s.expiresAt.getTime() <= expiresAtOrBefore.getTime() ||
      s.lastSeenAt.getTime() < lastSeenBefore.getTime();
    const cases = [
      { lastSeenAt: plus(now, -12 * H), expiresAt: plus(now, 1) },
      { lastSeenAt: plus(now, -12 * H - 1), expiresAt: plus(now, H) },
      { lastSeenAt: plus(now, -1), expiresAt: now },
      { lastSeenAt: plus(now, -1), expiresAt: plus(now, 1) },
    ];
    for (const s of cases) expect(dead(s)).toBe(!isSessionAlive(s, now));
  });
});
