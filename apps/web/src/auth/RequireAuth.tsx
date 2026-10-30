import type { ReactNode } from 'react';
import { Outlet, useLocation } from 'react-router';
import { EmptyState } from '@/components/EmptyState.tsx';
import { Skeleton } from '@/components/ui/skeleton';
import { strings } from '@/strings.ts';
import { useAuth } from './AuthProvider.tsx';
import { LoginPage } from './LoginPage.tsx';

/** Layout route: renders its children for a signed-in user, else the login page in place. */
export function RequireAuth() {
  const { status } = useAuth();
  const { pathname, search } = useLocation();
  if (status === 'loading') {
    return (
      <div aria-busy="true" className="flex flex-col gap-3 p-6">
        <span className="sr-only">{strings.common.loading}</span>
        <Skeleton className="h-8 w-48" />
        <Skeleton className="h-4 w-full max-w-md" />
      </div>
    );
  }
  if (status === 'signed-out') {
    const query = new URLSearchParams(search);
    query.delete('loginError'); // the failure of the previous attempt is not part of the target
    const rest = query.toString();
    return <LoginPage returnTo={rest ? `${pathname}?${rest}` : pathname} />;
  }
  return <Outlet />;
}

/** Admin-only content; members get "Access denied". Use inside `RequireAuth`. */
export function RequireAdmin({ children }: { children: ReactNode }) {
  const { me } = useAuth();
  if (me?.role !== 'admin') {
    return (
      <EmptyState title={strings.access.forbidden} description={strings.access.forbiddenHint} />
    );
  }
  return children;
}
