import { ConnectCallbackError, StartConnectResponse, type ConnectionSummary } from '@mcp/contracts';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { PlugIcon } from 'lucide-react';
import { useEffect, useState, type FormEvent } from 'react';
import { useSearchParams } from 'react-router';
import { api } from '@/api/client.ts';
import { queryKeys, useConnections } from '@/api/queries.ts';
import { ConfirmDialog } from '@/components/ConfirmDialog.tsx';
import { EmptyState } from '@/components/EmptyState.tsx';
import { ErrorAlert } from '@/components/ErrorAlert.tsx';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Skeleton } from '@/components/ui/skeleton';
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table';
import { redirectTo } from '@/lib/navigate.ts';
import { notify } from '@/lib/notify.ts';
import { strings } from '@/strings.ts';

const t = strings.connections;

function useStartConnect() {
  return useMutation({
    mutationFn: async (server: string) => {
      const { url } = await api.post(
        `/api/connections/${encodeURIComponent(server)}/start`,
        undefined,
        StartConnectResponse,
      );
      redirectTo(url);
    },
  });
}

function useSavePat() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ server, token }: { server: string; token: string }) =>
      api.put(`/api/connections/${encodeURIComponent(server)}/pat`, { token }),
    onSuccess: () => client.invalidateQueries({ queryKey: queryKeys.connections }),
  });
}

function useDisconnect() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (server: string) => api.delete(`/api/connections/${encodeURIComponent(server)}`),
    onSuccess: () => client.invalidateQueries({ queryKey: queryKeys.connections }),
  });
}

function statusVariant(status: ConnectionSummary['status']) {
  if (status === 'ready') return 'default';
  return status === 'expired' ? 'destructive' : 'secondary';
}

/** Token input of a `pat_user` server. The typed value is dropped as soon as it is saved. */
function PatForm({ connection }: { connection: ConnectionSummary }) {
  const [token, setToken] = useState('');
  const save = useSavePat();
  function submit(e: FormEvent) {
    e.preventDefault();
    if (token === '') return;
    save.mutate(
      { server: connection.server, token },
      {
        onSuccess: () => {
          setToken('');
          notify.success(t.tokenSaved(connection.name));
        },
      },
    );
  }
  return (
    <form className="flex flex-col gap-1" onSubmit={submit}>
      <div className="flex gap-2">
        <Input
          type="password"
          autoComplete="off"
          aria-label={t.tokenFor(connection.name)}
          placeholder={t.tokenHint}
          maxLength={4096}
          value={token}
          onChange={(e) => setToken(e.target.value)}
        />
        <Button
          type="submit"
          size="sm"
          aria-label={t.saveToken(connection.name)}
          disabled={save.isPending || token === ''}
        >
          {strings.common.save}
        </Button>
      </div>
      {save.isError && <ErrorAlert error={save.error} />}
    </form>
  );
}

function ConnectionRow({
  connection,
  onError,
}: {
  connection: ConnectionSummary;
  onError: (e: unknown) => void;
}) {
  const start = useStartConnect();
  const disconnect = useDisconnect();
  const { name, status } = connection;
  const oauth = connection.authKind === 'oauth_user';
  return (
    <TableRow>
      <TableCell className="font-medium">{name}</TableCell>
      <TableCell>
        <Badge variant={statusVariant(status)}>{t.status[status]}</Badge>
      </TableCell>
      <TableCell>
        <div className="flex flex-wrap items-start justify-end gap-2">
          {oauth ? (
            <Button
              size="sm"
              variant={status === 'ready' ? 'outline' : 'default'}
              aria-label={status === 'missing' ? t.connectAria(name) : t.reconnectAria(name)}
              disabled={start.isPending}
              onClick={() => start.mutate(connection.server, { onError })}
            >
              {status === 'missing' ? t.connect : t.reconnect}
            </Button>
          ) : (
            <div className="min-w-64 flex-1">
              <PatForm connection={connection} />
            </div>
          )}
          {status !== 'missing' && (
            <ConfirmDialog
              trigger={
                <Button size="sm" variant="outline" aria-label={t.disconnectAria(name)}>
                  {t.disconnect}
                </Button>
              }
              title={t.disconnectTitle(name)}
              description={t.disconnectDescription}
              confirmLabel={t.disconnect}
              destructive
              onConfirm={async () => {
                await disconnect.mutateAsync(connection.server);
                notify.success(t.disconnected(name));
              }}
            />
          )}
        </div>
      </TableCell>
    </TableRow>
  );
}

/** `/connections`: the person's own credentials to the upstream servers that need them. */
export function ConnectionsPage() {
  const connections = useConnections();
  const [params, setParams] = useSearchParams();
  const [failure, setFailure] = useState<unknown>(null);
  const [callbackError, setCallbackError] = useState<string | null>(null);
  const connectedSlug = params.get('connected');
  const errorCode = params.get('error');

  // The callback lands here with `?connected=` or `?error=`; show it once, then clean the URL.
  useEffect(() => {
    if (connectedSlug === null && errorCode === null) return;
    if (errorCode !== null) {
      const known = ConnectCallbackError.safeParse(errorCode);
      setCallbackError(t.errors[known.success ? known.data : 'exchange_failed']);
    }
    if (connectedSlug !== null) {
      if (connections.isPending) return; // wait for the list to name the server
      const name = connections.data?.find((c) => c.server === connectedSlug)?.name ?? connectedSlug;
      notify.success(t.connected(name));
    }
    setParams({}, { replace: true });
  }, [connectedSlug, errorCode, connections.data, connections.isPending, setParams]);

  let body;
  if (connections.isError) body = <ErrorAlert error={connections.error} />;
  else if (connections.data === undefined) body = <Skeleton className="h-32 w-full" />;
  else if (connections.data.length === 0) {
    body = <EmptyState icon={PlugIcon} title={t.empty} description={t.emptyHint} />;
  } else {
    body = (
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>{strings.servers.columns.name}</TableHead>
            <TableHead>{strings.servers.columns.status}</TableHead>
            <TableHead className="text-right">
              <span className="sr-only">{strings.tokens.columns.actions}</span>
            </TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {connections.data.map((c) => (
            <ConnectionRow key={c.server} connection={c} onError={setFailure} />
          ))}
        </TableBody>
      </Table>
    );
  }

  return (
    <div className="mx-auto flex w-full max-w-4xl flex-col gap-4">
      <h1 className="text-2xl font-semibold">{t.title}</h1>
      <p className="text-sm text-muted-foreground">{t.hint}</p>
      {callbackError !== null && <ErrorAlert title={t.errorTitle} message={callbackError} />}
      {failure !== null && <ErrorAlert title={t.errorTitle} error={failure} />}
      {body}
    </div>
  );
}
