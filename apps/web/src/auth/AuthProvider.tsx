import { MeResponse } from '@mcp/contracts';
import { useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query';
import { createContext, use, useEffect, useMemo, type ReactNode } from 'react';
import { api, onUnauthorized } from '@/api/client.ts';
import { isApiError } from '@/api/errors.ts';

export interface Auth {
  me: MeResponse | null;
  status: 'loading' | 'signed-in' | 'signed-out';
  /** Ends the session on the server, then drops everything cached. */
  logout: () => Promise<void>;
}

const ME_KEY = ['auth', 'me'] as const;

/**
 * Drops every cached query of the previous session except `me` (the AuthProvider observes that
 * query; removing it would detach the observer), then sets `me`.
 */
function resetSession(queryClient: QueryClient, me: MeResponse | null) {
  queryClient.removeQueries({ predicate: (q) => q.queryKey[0] !== ME_KEY[0] });
  queryClient.setQueryData(ME_KEY, me);
}

/** The signed-in user, or null: a 401 from `/me` means "signed out", not an error. */
async function fetchMe(): Promise<MeResponse | null> {
  try {
    return await api.get('/api/auth/me', MeResponse);
  } catch (e) {
    if (isApiError(e) && e.status === 401) return null;
    throw e;
  }
}

const AuthContext = createContext<Auth | null>(null);

/**
 * Session state for the whole app. Sign-in is a full-page redirect to the OIDC provider, so there is no
 * login call here: a 401 from any API call just flips the state to signed-out, and the guard
 * shows the login page.
 */
export function AuthProvider({ children }: { children: ReactNode }) {
  const queryClient = useQueryClient();
  const query = useQuery({ queryKey: ME_KEY, queryFn: fetchMe, staleTime: Infinity });

  useEffect(
    () =>
      onUnauthorized(() => {
        resetSession(queryClient, null);
      }),
    [queryClient],
  );

  const me = query.data ?? null;
  const status: Auth['status'] = query.isPending ? 'loading' : me ? 'signed-in' : 'signed-out';

  const value = useMemo<Auth>(
    () => ({
      me,
      status,
      async logout() {
        await queryClient.cancelQueries();
        try {
          await api.post('/api/auth/logout');
        } catch (e) {
          if (!isApiError(e) || e.status !== 401) {
            // The session may still be alive: show the real state and let the caller report.
            await queryClient.refetchQueries({ queryKey: ME_KEY });
            throw e;
          }
        }
        resetSession(queryClient, null);
      },
    }),
    [me, status, queryClient],
  );

  return <AuthContext value={value}>{children}</AuthContext>;
}

export function useAuth(): Auth {
  const auth = use(AuthContext);
  if (!auth) throw new Error('useAuth must be used inside <AuthProvider>');
  return auth;
}
