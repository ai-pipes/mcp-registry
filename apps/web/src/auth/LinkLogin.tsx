import { useQueryClient } from '@tanstack/react-query';
import { useEffect, useRef, useState } from 'react';
import { Outlet, useLocation, useNavigate } from 'react-router';
import { api } from '@/api/client.ts';
import { isApiError } from '@/api/errors.ts';
import { Skeleton } from '@/components/ui/skeleton';
import { strings } from '@/strings.ts';
import { reloadSession } from './AuthProvider.tsx';

const PREFIX = '#link=';

function codeFrom(hash: string): string | null {
  if (!hash.startsWith(PREFIX)) return null;
  let code: string;
  try {
    code = decodeURIComponent(hash.slice(PREFIX.length));
  } catch {
    return null;
  }
  return code === '' ? null : code;
}

/**
 * Exchanges the one-time code of `<origin>/login#link=<code>` for a session. Sits above
 * `RequireAuth`, so it works with or without a session. The fragment is removed from the address at
 * once (the code never stays in the URL or in the history) and the code is sent exactly once.
 * Success lands on the catalog; a rejected link goes to `/?loginError=<code>`, which shows the
 * login page for a signed-out visitor and silently opens the catalog for a signed-in one.
 */
export function LinkLogin() {
  const { hash, pathname, search } = useLocation();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const code = codeFrom(hash);
  const [sending, setSending] = useState(code !== null);
  // Where the router was sent after the request. The router applies a navigation in a React
  // transition, so the outlet must stay hidden until the location really changed, or the route
  // we are leaving (`/login`) would render once more and redirect on its own.
  const [target, setTarget] = useState<string | null>(null);
  const arrived = target !== null && pathname + search === target;
  const busy = sending || (target !== null && !arrived);

  useEffect(() => {
    if (arrived) setTarget(null);
  }, [arrived]);
  const inFlight = useRef<string | null>(null);

  useEffect(() => {
    // StrictMode runs this effect twice before the router drops the fragment: guard on the ref.
    if (code === null || inFlight.current === code) return;
    inFlight.current = code;
    setSending(true);
    void navigate({ pathname, search }, { replace: true });
    void (async () => {
      let next = '/catalog';
      try {
        await api.post('/api/auth/link', { code });
        await reloadSession(queryClient);
      } catch (e) {
        next = `/?loginError=${encodeURIComponent(isApiError(e) ? e.code : 'internal')}`;
      }
      inFlight.current = null;
      setTarget(next);
      setSending(false);
      void navigate(next, { replace: true });
    })();
  }, [code, pathname, search, navigate, queryClient]);

  if (busy) {
    return (
      <div aria-busy="true" className="flex flex-col gap-3 p-6">
        <span className="sr-only">{strings.common.loading}</span>
        <Skeleton className="h-8 w-48" />
      </div>
    );
  }
  return <Outlet />;
}
