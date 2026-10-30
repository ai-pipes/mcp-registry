import { type UserSummary } from '@mcp/contracts';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { api } from '@/api/client.ts';
import { queryKeys, useUsers } from '@/api/queries.ts';
import { ConfirmDialog } from '@/components/ConfirmDialog.tsx';
import { ErrorAlert } from '@/components/ErrorAlert.tsx';
import { Badge } from '@/components/ui/badge';
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
import { notify } from '@/lib/notify.ts';
import { strings } from '@/strings.ts';

const t = strings.users;

function useSetDisabled() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ id, disabled }: { id: string; disabled: boolean }) =>
      api.post(`/api/users/${encodeURIComponent(id)}/disabled`, { disabled }),
    onSuccess: (_, { disabled }) => {
      notify.success(disabled ? t.disabledToast : t.enabledToast);
      return client.invalidateQueries({ queryKey: queryKeys.users });
    },
  });
}

function UserRow({ user }: { user: UserSummary }) {
  const setDisabled = useSetDisabled();
  return (
    <TableRow>
      <TableCell className="font-medium">{user.name}</TableCell>
      <TableCell>{user.email ?? strings.common.empty}</TableCell>
      <TableCell>
        {user.role === 'admin' ? strings.nav.roleAdmin : strings.nav.roleMember}
      </TableCell>
      <TableCell className="whitespace-normal text-xs">{user.groups.join(', ')}</TableCell>
      <TableCell>
        {user.lastLoginAt ? (
          <time dateTime={user.lastLoginAt}>{formatDateTime(user.lastLoginAt)}</time>
        ) : (
          <span className="text-muted-foreground">{strings.common.never}</span>
        )}
      </TableCell>
      <TableCell>
        <Badge variant={user.disabled ? 'secondary' : 'outline'}>
          {user.disabled ? t.disabled : t.active}
        </Badge>
      </TableCell>
      <TableCell className="text-right">
        {user.disabled ? (
          <Button
            size="sm"
            variant="outline"
            disabled={setDisabled.isPending}
            onClick={() => setDisabled.mutate({ id: user.id, disabled: false })}
          >
            {t.enable}
          </Button>
        ) : (
          <ConfirmDialog
            trigger={
              <Button size="sm" variant="outline">
                {t.disable}
              </Button>
            }
            title={t.disableTitle(user.name)}
            description={t.disableDescription}
            confirmLabel={t.disable}
            destructive
            onConfirm={() => setDisabled.mutateAsync({ id: user.id, disabled: true })}
          />
        )}
      </TableCell>
    </TableRow>
  );
}

/** `/admin/users`: people who have signed in, and the switch that disables an account. */
export function UsersPage() {
  const users = useUsers();
  let body;
  if (users.isError) body = <ErrorAlert error={users.error} />;
  else if (users.data === undefined) body = <Skeleton className="h-32 w-full" />;
  else if (users.data.length === 0) body = <p className="text-muted-foreground">{t.empty}</p>;
  else {
    body = (
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>{t.columns.name}</TableHead>
            <TableHead>{t.columns.email}</TableHead>
            <TableHead>{t.columns.role}</TableHead>
            <TableHead>{t.columns.groups}</TableHead>
            <TableHead>{t.columns.lastLogin}</TableHead>
            <TableHead>{t.columns.status}</TableHead>
            <TableHead className="text-right">
              <span className="sr-only">{t.columns.actions}</span>
            </TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {users.data.map((u) => (
            <UserRow key={u.id} user={u} />
          ))}
        </TableBody>
      </Table>
    );
  }
  return (
    <div className="mx-auto flex w-full max-w-5xl flex-col gap-4">
      <h1 className="text-2xl font-semibold">{t.title}</h1>
      {body}
    </div>
  );
}
