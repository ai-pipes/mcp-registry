import { PlusIcon, ServerIcon } from 'lucide-react';
import { Link } from 'react-router';
import { useServers } from '@/api/queries.ts';
import { EmptyState } from '@/components/EmptyState.tsx';
import { ErrorAlert } from '@/components/ErrorAlert.tsx';
import { Badge } from '@/components/ui/badge';
import { buttonVariants } from '@/components/ui/button';
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
import { cn } from '@/lib/utils.ts';
import { strings } from '@/strings.ts';

const t = strings.servers;

/** `/admin/servers`: all upstream servers. */
export function ServersPage() {
  const servers = useServers();
  const create = (
    <Link to="/admin/servers/new" className={cn(buttonVariants())}>
      <PlusIcon aria-hidden className="size-4" />
      {t.create}
    </Link>
  );
  let body;
  if (servers.isError) body = <ErrorAlert error={servers.error} />;
  else if (servers.data === undefined) body = <Skeleton className="h-32 w-full" />;
  else if (servers.data.length === 0) {
    body = (
      <EmptyState icon={ServerIcon} title={t.empty} description={t.emptyHint} action={create} />
    );
  } else {
    body = (
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>{t.columns.name}</TableHead>
            <TableHead>{t.columns.auth}</TableHead>
            <TableHead>{t.columns.status}</TableHead>
            <TableHead>{t.columns.tools}</TableHead>
            <TableHead>{t.columns.sync}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {servers.data.map((s) => (
            <TableRow key={s.slug}>
              <TableCell>
                <Link
                  to={`/admin/servers/${encodeURIComponent(s.slug)}`}
                  className="font-medium underline-offset-4 hover:underline"
                >
                  {s.name}
                </Link>
                <div className="font-mono text-xs text-muted-foreground">{s.slug}</div>
              </TableCell>
              <TableCell>{t.authKind[s.auth.kind]}</TableCell>
              <TableCell>
                <Badge variant={s.status === 'active' ? 'outline' : 'secondary'}>
                  {t.status[s.status]}
                </Badge>
              </TableCell>
              <TableCell>{s.toolCount}</TableCell>
              <TableCell>
                {s.lastSyncError ? (
                  <Badge variant="destructive">{t.syncFailed}</Badge>
                ) : s.lastSyncAt ? (
                  <time dateTime={s.lastSyncAt}>{formatDateTime(s.lastSyncAt)}</time>
                ) : (
                  <span className="text-muted-foreground">{t.syncNever}</span>
                )}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    );
  }
  return (
    <div className="mx-auto flex w-full max-w-5xl flex-col gap-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h1 className="text-2xl font-semibold">{t.title}</h1>
        {servers.data && servers.data.length > 0 && create}
      </div>
      {body}
    </div>
  );
}
