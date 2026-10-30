import { CallList, CallOutcome, DEFAULT_CALLS_LIMIT } from '@mcp/contracts';
import { useInfiniteQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { api } from '@/api/client.ts';
import { useServers, useUsers } from '@/api/queries.ts';
import { ErrorAlert } from '@/components/ErrorAlert.tsx';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Label } from '@/components/ui/label';
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
const SELECT_CLASS =
  'h-8 rounded-lg border border-input bg-transparent px-2.5 text-sm outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50';

/** `/admin/calls`: the call log (who, which tool, how it ended), newest first. */
export function CallsPage() {
  const [server, setServer] = useState('');
  const [outcome, setOutcome] = useState('');
  const servers = useServers();
  const users = useUsers();
  const calls = useInfiniteQuery({
    queryKey: ['calls', server, outcome],
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam }) => {
      const q = new URLSearchParams({ limit: String(DEFAULT_CALLS_LIMIT) });
      if (server) q.set('server', server);
      if (outcome) q.set('outcome', outcome);
      if (pageParam) q.set('before', pageParam);
      return api.get(`/api/calls?${q.toString()}`, CallList);
    },
    getNextPageParam: (last) =>
      last.length < DEFAULT_CALLS_LIMIT ? undefined : last[last.length - 1]?.startedAt,
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
          <select
            id="calls-server"
            className={SELECT_CLASS}
            value={server}
            onChange={(e) => setServer(e.target.value)}
          >
            <option value="">{t.allOutcomes}</option>
            {(servers.data ?? []).map((s) => (
              <option key={s.slug} value={s.slug}>
                {s.name}
              </option>
            ))}
          </select>
        </div>
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="calls-outcome">{t.filterOutcome}</Label>
          <select
            id="calls-outcome"
            className={SELECT_CLASS}
            value={outcome}
            onChange={(e) => setOutcome(e.target.value)}
          >
            <option value="">{t.allOutcomes}</option>
            {CallOutcome.options.map((o) => (
              <option key={o} value={o}>
                {t.outcome[o]}
              </option>
            ))}
          </select>
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
