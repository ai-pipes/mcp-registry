const DAY_MS = 86_400_000;

/**
 * The day a daily limit counts in: the UTC calendar day (`YYYY-MM-DD`). It resets at the next
 * UTC midnight, whatever the server's or the person's time zone.
 */
export const QuotaDay = {
  of(now: Date): { day: string; resetAt: Date } {
    const day = now.toISOString().slice(0, 10);
    return { day, resetAt: QuotaDay.resetAt(day) };
  },

  /** The oldest day whose counter is kept: yesterday (UTC). Older counters no longer matter. */
  oldestKept(now: Date): string {
    return new Date(now.getTime() - DAY_MS).toISOString().slice(0, 10);
  },

  /** The UTC midnight that ends `day` (`YYYY-MM-DD`). */
  resetAt(day: string): Date {
    return new Date(Date.parse(`${day}T00:00:00Z`) + DAY_MS);
  },
};
