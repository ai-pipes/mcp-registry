import { strings } from '@/strings.ts';

const { locale } = strings.format;

/** Absolute date and time. */
export function formatDateTime(iso: string): string {
  return new Intl.DateTimeFormat(locale, { dateStyle: 'medium', timeStyle: 'short' }).format(
    new Date(iso),
  );
}
