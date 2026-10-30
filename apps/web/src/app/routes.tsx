import { Navigate, Outlet, type RouteObject } from 'react-router';
import { RequireAdmin, RequireAuth } from '@/auth/RequireAuth.tsx';
import { AuthProvider } from '@/auth/AuthProvider.tsx';
import { CatalogPage } from '@/catalog/CatalogPage.tsx';
import { AuditPage } from '@/admin/audit/AuditPage.tsx';
import { CallsPage } from '@/admin/calls/CallsPage.tsx';
import { ServerPage } from '@/admin/servers/ServerPage.tsx';
import { ServersPage } from '@/admin/servers/ServersPage.tsx';
import { UsersPage } from '@/admin/users/UsersPage.tsx';
import { EmptyState } from '@/components/EmptyState.tsx';
import { ConnectionsPage } from '@/connections/ConnectionsPage.tsx';
import { TokensPage } from '@/tokens/TokensPage.tsx';
import { strings } from '@/strings.ts';
import { Layout } from './Layout.tsx';

/**
 * `/` → `/catalog`. Everything sits behind `RequireAuth` (a signed-out visitor sees the login
 * page at the same URL, so `/?loginError=` from the OIDC callback lands on it); `/admin/**` is
 * additionally behind `RequireAdmin` (members get "Access denied" and no menu items).
 */
export const routes: RouteObject[] = [
  {
    element: (
      <AuthProvider>
        <Outlet />
      </AuthProvider>
    ),
    children: [
      {
        element: <RequireAuth />,
        children: [
          {
            element: <Layout />,
            children: [
              { index: true, element: <Navigate to="/catalog" replace /> },
              { path: 'catalog', element: <CatalogPage /> },
              { path: 'connections', element: <ConnectionsPage /> },
              { path: 'tokens', element: <TokensPage /> },
              {
                path: 'admin',
                element: (
                  <RequireAdmin>
                    <Outlet />
                  </RequireAdmin>
                ),
                children: [
                  { index: true, element: <Navigate to="/admin/servers" replace /> },
                  { path: 'servers', element: <ServersPage /> },
                  { path: 'servers/new', element: <ServerPage /> },
                  { path: 'servers/:slug', element: <ServerPage /> },
                  { path: 'users', element: <UsersPage /> },
                  { path: 'calls', element: <CallsPage /> },
                  { path: 'audit', element: <AuditPage /> },
                ],
              },
              { path: '*', element: <EmptyState title={strings.access.notFound} /> },
            ],
          },
        ],
      },
    ],
  },
];
