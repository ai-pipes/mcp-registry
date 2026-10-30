import { SyncResult } from '@mcp/contracts';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { RefreshCwIcon } from 'lucide-react';
import { Link, useNavigate, useParams } from 'react-router';
import { api } from '@/api/client.ts';
import { isApiError } from '@/api/errors.ts';
import { queryKeys, useServer, useTools } from '@/api/queries.ts';
import { EmptyState } from '@/components/EmptyState.tsx';
import { ErrorAlert } from '@/components/ErrorAlert.tsx';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Skeleton } from '@/components/ui/skeleton';
import { notify } from '@/lib/notify.ts';
import { strings } from '@/strings.ts';
import { ServerForm } from './ServerForm.tsx';
import { ToolsTable } from './ToolsTable.tsx';

const t = strings.servers;

function BackLink() {
  return (
    <Link to="/admin/servers" className="text-sm text-muted-foreground hover:underline">
      ← {t.back}
    </Link>
  );
}

function useServerActions(slug: string) {
  const client = useQueryClient();
  const base = `/api/servers/${encodeURIComponent(slug)}`;
  const refresh = () =>
    Promise.all([
      client.invalidateQueries({ queryKey: queryKeys.servers }),
      client.invalidateQueries({ queryKey: queryKeys.server(slug) }),
      client.invalidateQueries({ queryKey: queryKeys.tools(slug) }),
    ]);
  const sync = useMutation({
    mutationFn: () => api.post(`${base}/sync`, undefined, SyncResult),
    onSuccess: async (r) => {
      notify.success(t.syncResult(r.added, r.changed, r.removed));
      await refresh();
    },
    // A failed sync is recorded as `lastSyncError`; refetch to show it.
    onError: () => void refresh(),
  });
  const setStatus = useMutation({
    mutationFn: (status: 'active' | 'disabled') => api.post(`${base}/status`, { status }),
    onSuccess: async () => {
      notify.success(t.statusChanged);
      await refresh();
    },
  });
  return { sync, setStatus };
}

function ExistingServer({ slug }: { slug: string }) {
  const server = useServer(slug);
  const tools = useTools(slug);
  const { sync, setStatus } = useServerActions(slug);

  if (server.isError) {
    return isApiError(server.error) && server.error.status === 404 ? (
      <EmptyState title={t.notFound} />
    ) : (
      <ErrorAlert error={server.error} />
    );
  }
  if (server.data === undefined) return <Skeleton className="h-48 w-full" />;
  const s = server.data;
  const active = s.status === 'active';

  return (
    <>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h1 className="flex items-center gap-2 text-2xl font-semibold">
          {s.name}
          <Badge variant={active ? 'outline' : 'secondary'}>{t.status[s.status]}</Badge>
        </h1>
        <div className="flex gap-2">
          <Button
            variant="outline"
            disabled={setStatus.isPending}
            onClick={() => setStatus.mutate(active ? 'disabled' : 'active')}
          >
            {active ? t.disable : t.enable}
          </Button>
          <Button disabled={sync.isPending} onClick={() => sync.mutate()}>
            <RefreshCwIcon aria-hidden />
            {sync.isPending ? t.syncing : t.sync}
          </Button>
        </div>
      </div>
      {setStatus.isError && <ErrorAlert error={setStatus.error} />}
      {sync.isError && <ErrorAlert title={t.syncErrorFailed} error={sync.error} />}
      {s.lastSyncError && !sync.isError && (
        <ErrorAlert title={t.syncErrorTitle} message={s.lastSyncError} />
      )}

      <section className="flex flex-col gap-3">
        <h2 className="text-lg font-medium">{t.toolsTitle}</h2>
        {tools.isError ? (
          <ErrorAlert error={tools.error} />
        ) : tools.data === undefined ? (
          <Skeleton className="h-24 w-full" />
        ) : tools.data.length === 0 ? (
          <p className="text-sm text-muted-foreground">{t.noTools}</p>
        ) : (
          <ToolsTable slug={slug} tools={tools.data} />
        )}
      </section>

      <section className="flex flex-col gap-3">
        <h2 className="text-lg font-medium">{t.edit}</h2>
        <ServerForm server={s} onSaved={() => undefined} />
      </section>
    </>
  );
}

/** `/admin/servers/:slug` (one server: sync, tools, settings) and `/admin/servers/new`. */
export function ServerPage() {
  const { slug } = useParams();
  const navigate = useNavigate();
  return (
    <div className="mx-auto flex w-full max-w-4xl flex-col gap-4">
      <BackLink />
      {slug === undefined ? (
        <>
          <h1 className="text-2xl font-semibold">{t.createTitle}</h1>
          <ServerForm
            onSaved={(created) =>
              void navigate(`/admin/servers/${encodeURIComponent(created.slug)}`)
            }
          />
        </>
      ) : (
        <ExistingServer slug={slug} />
      )}
    </div>
  );
}
