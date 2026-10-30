import { describe, expect, it } from 'vite-plus/test';
import { QuotaDay } from './quota-day.ts';

describe('QuotaDay', () => {
  it('is the UTC calendar day, reset at the next UTC midnight', () => {
    expect(QuotaDay.of(new Date('2026-10-30T13:45:00Z'))).toEqual({
      day: '2026-10-30',
      resetAt: new Date('2026-10-31T00:00:00Z'),
    });
  });

  it('ignores the local offset: 23:30 UTC is still the same UTC day', () => {
    expect(QuotaDay.of(new Date('2026-10-30T23:30:00-00:00')).day).toBe('2026-10-30');
    expect(QuotaDay.of(new Date('2026-10-31T01:30:00+02:00')).day).toBe('2026-10-30');
  });

  it('midnight itself starts the new day', () => {
    expect(QuotaDay.of(new Date('2026-10-31T00:00:00Z')).day).toBe('2026-10-31');
  });

  it('crosses month and year boundaries', () => {
    expect(QuotaDay.of(new Date('2026-12-31T22:00:00Z'))).toEqual({
      day: '2026-12-31',
      resetAt: new Date('2027-01-01T00:00:00Z'),
    });
  });

  it('resetAt of a stored day is the following UTC midnight', () => {
    expect(QuotaDay.resetAt('2028-02-28')).toEqual(new Date('2028-02-29T00:00:00Z'));
  });

  it('oldestKept: yesterday (UTC) — today and yesterday are kept, older days may go', () => {
    expect(QuotaDay.oldestKept(new Date('2026-10-30T00:30:00Z'))).toBe('2026-10-29');
    expect(QuotaDay.oldestKept(new Date('2026-01-01T23:00:00Z'))).toBe('2025-12-31');
  });
});
