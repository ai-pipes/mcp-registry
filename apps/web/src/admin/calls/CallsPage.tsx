import { CallList, CallOutcome, DEFAULT_CALLS_LIMIT } from '@mcp/contracts';
import { useInfiniteQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { api } from '@/api/client.ts';
import { useServers, useUsers } from '@/api/queries.ts';
import { ErrorAlert } from '@/components/ErrorAlert.tsx';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Label } from '@/components/ui/label';
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select';
import { Skeleton } from '@/components/ui/skeleton';
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table';
import { formatDateTime } from '@/lib/format.ts';
import { strings } from '@/strings.ts';

const t = strings.calls;
const ALL = 'all';

/**
 * A filter dropdown with an "All" entry (empty value). Base UI Select, not a native `<select>`:
 * WebKit reports a `style-src-attr` violation for every native `<select>` under the strict CSP
 * (its internal inline style is checked against the page policy), which e2e/csp.spec.ts forbids.
 */
function FilterSelect({
  id,
  value,
  onChange,
  items,
}: {
  id: string;
  value: string;
  onChange: (value: string) => void;
  items: { value: string; label: string }[];
}) {
  const all = [{ value: ALL, label: t.allOutcomes }, ...items];
  return (
    <Select
      value={value === '' ? ALL : value}
      onValueChange={(v) => onChange(v === ALL ? '' : String(v))}
      items={all}
    >
      <SelectTrigger id={id} size="sm" className="min-w-40">
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        {all.map((i) => (
          <SelectItem key={i.value} value={i.value}>
            {i.label}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );
}

/** `/admin/calls`: the call log (who, which tool, how it ended), newest first. */
export function CallsPage() {
  const [server, setServer] = useState('');
  const [outcome, setOutcome] = useState('');
  const servers = useServers();
  const users = useUsers();
  const calls = useInfiniteQuery({
    queryKey: ['calls', server, outcome],
    initialPageParam: undefined as { before: string; beforeId: string } | undefined,
    queryFn: ({ pageParam }) => {
      const q = new URLSearchParams({ limit: String(DEFAULT_CALLS_LIMIT) });
      if (server) q.set('server', server);
      if (outcome) q.set('outcome', outcome);
      if (pageParam) {
        q.set('before', pageParam.before);
        q.set('beforeId', pageParam.beforeId);
      }
      return api.get(`/api/calls?${q.toString()}`, CallList);
    },
    getNextPageParam: (last) => {
      const tail = last[last.length - 1];
      return last.length < DEFAULT_CALLS_LIMIT || tail === undefined
        ? undefined
        : { before: tail.startedAt, beforeId: tail.id };
    },
  });
  const names = new Map((users.data ?? []).map((u) => [u.id, u.name]));
  const rows = calls.data?.pages.flat() ?? [];

  return (
    <div className="mx-auto flex w-full max-w-5xl flex-col gap-4">
      <h1 className="text-2xl font-semibold">{t.title}</h1>
      <p className="text-sm text-muted-foreground">{t.hint}</p>
      <div className="flex flex-wrap gap-4">
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="calls-server">{t.filterServer}</Label>
          <FilterSelect
            id="calls-server"
            value={server}
            onChange={setServer}
            items={(servers.data ?? []).map((s) => ({ value: s.slug, label: s.name }))}
          />
        </div>
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="calls-outcome">{t.filterOutcome}</Label>
          <FilterSelect
            id="calls-outcome"
            value={outcome}
            onChange={setOutcome}
            items={CallOutcome.options.map((o) => ({ value: o, label: t.outcome[o] ?? o }))}
          />
        </div>
      </div>
      {calls.isError ? (
        <ErrorAlert error={calls.error} />
      ) : calls.data === undefined ? (
        <Skeleton className="h-32 w-full" />
      ) : rows.length === 0 ? (
        <p className="text-muted-foreground">{t.empty}</p>
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{t.columns.at}</TableHead>
              <TableHead>{t.columns.user}</TableHead>
              <TableHead>{t.columns.server}</TableHead>
              <TableHead>{t.columns.tool}</TableHead>
              <TableHead>{t.columns.duration}</TableHead>
              <TableHead>{t.columns.outcome}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((c) => (
              <TableRow key={c.id}>
                <TableCell>
                  <time dateTime={c.startedAt}>{formatDateTime(c.startedAt)}</time>
                </TableCell>
                <TableCell>{names.get(c.userId) ?? c.userId}</TableCell>
                <TableCell className="font-mono text-xs">{c.server}</TableCell>
                <TableCell className="font-mono text-xs">{c.tool}</TableCell>
                <TableCell>{t.ms(c.durationMs)}</TableCell>
                <TableCell>
                  <Badge variant={c.outcome === 'ok' ? 'outline' : 'secondary'}>
                    {t.outcome[c.outcome]}
                  </Badge>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
      {calls.hasNextPage && (
        <div>
          <Button
            variant="outline"
            disabled={calls.isFetchingNextPage}
            onClick={() => void calls.fetchNextPage()}
          >
            {t.loadMore}
          </Button>
        </div>
      )}
    </div>
  );
}
