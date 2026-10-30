import { CircleAlertIcon } from 'lucide-react';
import type { ReactNode } from 'react';
import { isApiError } from '@/api/errors.ts';
import { Card, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { cn } from '@/lib/utils.ts';
import { strings } from '@/strings.ts';

/**
 * Russian text for a failed call. The client's own codes carry Russian messages; so do the
 * server's catalog-sync errors; other server messages are English, so they are replaced by the
 * generic HTTP text, unless the code has its own text (`strings.errors.codes`). Callers that know
 * a code pass their own `message`.
 */
const RUSSIAN_SERVER_CODES = new Set(['needs_connection', 'sync_failed']);

function describeError(error: unknown): string {
  if (!isApiError(error)) return strings.errors.unknown;
  const known = strings.errors.codes[error.code];
  if (known !== undefined) return known;
  if (
    error.code === 'network_error' ||
    error.code === 'bad_response' ||
    RUSSIAN_SERVER_CODES.has(error.code)
  ) {
    return error.message;
  }
  return strings.errors.http(error.status);
}

export interface ErrorAlertProps {
  /** The failure; its text comes from `describeError` unless `message` is given. */
  error?: unknown;
  message?: ReactNode;
  title?: string;
  className?: string;
}

/** Page-level error (form errors go under the field, success into a toast). */
export function ErrorAlert({ error, message, title, className }: ErrorAlertProps) {
  return (
    <Card role="alert" className={cn('border-destructive/50', className)}>
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-destructive">
          <CircleAlertIcon aria-hidden className="size-4" />
          {title ?? strings.errors.title}
        </CardTitle>
        <CardDescription>{message ?? describeError(error)}</CardDescription>
      </CardHeader>
    </Card>
  );
}
