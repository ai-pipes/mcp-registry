import { AuditList } from '@mcp/contracts';
import { useInfiniteQuery } from '@tanstack/react-query';
import { api } from '@/api/client.ts';
import { useUsers } from '@/api/queries.ts';
import { ErrorAlert } from '@/components/ErrorAlert.tsx';
import { Button } from '@/components/ui/button';
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

const t = strings.audit;
const PAGE = 100;

/** `/admin/audit`: administrator actions, newest first. */
export function AuditPage() {
  const users = useUsers();
  const audit = useInfiniteQuery({
    queryKey: ['audit'],
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam }) => {
      const q = new URLSearchParams({ limit: String(PAGE) });
      if (pageParam) q.set('before', pageParam);
      return api.get(`/api/audit?${q.toString()}`, AuditList);
    },
    getNextPageParam: (last) => (last.length < PAGE ? undefined : last[last.length - 1]?.id),
  });
  const names = new Map((users.data ?? []).map((u) => [u.id, u.name]));
  const rows = audit.data?.pages.flat() ?? [];

  return (
    <div className="mx-auto flex w-full max-w-5xl flex-col gap-4">
      <h1 className="text-2xl font-semibold">{t.title}</h1>
      <p className="text-sm text-muted-foreground">{t.hint}</p>
      {audit.isError ? (
        <ErrorAlert error={audit.error} />
      ) : audit.data === undefined ? (
        <Skeleton className="h-32 w-full" />
      ) : rows.length === 0 ? (
        <p className="text-muted-foreground">{t.empty}</p>
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{t.columns.at}</TableHead>
              <TableHead>{t.columns.actor}</TableHead>
              <TableHead>{t.columns.action}</TableHead>
              <TableHead>{t.columns.target}</TableHead>
              <TableHead>{t.columns.meta}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((e) => (
              <TableRow key={e.id}>
                <TableCell>
                  <time dateTime={e.at}>{formatDateTime(e.at)}</time>
                </TableCell>
                <TableCell>
                  {e.actorId === null ? t.system : (names.get(e.actorId) ?? e.actorId)}
                </TableCell>
                <TableCell className="font-mono text-xs">{e.action}</TableCell>
                <TableCell className="font-mono text-xs">
                  {e.target ?? strings.common.empty}
                </TableCell>
                <TableCell className="whitespace-normal font-mono text-xs">
                  {Object.keys(e.meta).length === 0 ? strings.common.empty : JSON.stringify(e.meta)}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
      {audit.hasNextPage && (
        <div>
          <Button
            variant="outline"
            disabled={audit.isFetchingNextPage}
            onClick={() => void audit.fetchNextPage()}
          >
            {t.loadMore}
          </Button>
        </div>
      )}
    </div>
  );
}
