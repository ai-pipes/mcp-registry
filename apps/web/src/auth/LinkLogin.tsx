import { useQueryClient } from '@tanstack/react-query';
import { useEffect, useEffectEvent, useRef, useState } from 'react';
import { Outlet, useLocation, useNavigate } from 'react-router';
import { api } from '@/api/client.ts';
import { isApiError } from '@/api/errors.ts';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card';
import { Skeleton } from '@/components/ui/skeleton';
import { useSystemTheme } from '@/lib/useSystemTheme.ts';
import { strings } from '@/strings.ts';
import { reloadSession, useAuth } from './AuthProvider.tsx';

const PREFIX = '#link=';
const LOGIN_PATH = '/login';

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

type Phase = 'idle' | 'waiting' | 'confirm' | 'sending';

/**
 * Exchanges the one-time code of `<origin>/login#link=<code>` for a session. Sits above
 * `RequireAuth`. The fragment is honoured only on `/login` and is removed from the address at
 * once (the code never stays in the URL or the history). Nobody signed in: the code is sent
 * exactly once; success lands on the catalog, a rejected link goes to `/?loginError=<code>`.
 * Somebody signed in: nothing is sent until the person confirms, so a link can never swap a
 * session silently (login CSRF); a declined or failed link leaves the session as it was.
 */
export function LinkLogin() {
  const { hash, pathname, search } = useLocation();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { status, me, sessionUnknown } = useAuth();
  const code = pathname === LOGIN_PATH ? codeFrom(hash) : null;
  const [phase, setPhase] = useState<Phase>(code !== null ? 'waiting' : 'idle');
  // Where the router was sent. The router applies a navigation in a React transition, so the
  // outlet stays hidden until the location really changed, or the route we are leaving (`/login`)
  // would render once more and redirect on its own.
  const [target, setTarget] = useState<string | null>(null);
  const arrived = target !== null && pathname + search === target;
  // The code lives only here, from the moment it is read until it is sent or dropped.
  const held = useRef<string | null>(null);
  // Set when the code is taken, cleared once the fragment is gone: a code is read at most once
  // however often effects re-run (StrictMode, a re-render before the router drops the fragment).
  const taken = useRef(false);

  useEffect(() => {
    if (arrived) setTarget(null);
  }, [arrived]);

  useEffect(() => {
    if (code === null) {
      taken.current = false;
      return;
    }
    if (taken.current) return;
    taken.current = true;
    held.current = code;
    setPhase('waiting');
    void navigate({ pathname, search }, { replace: true });
  }, [code, pathname, search, navigate]);

  function finish(next: string) {
    setTarget(next);
    setPhase('idle');
    void navigate(next, { replace: true });
  }

  const send = useEffectEvent(async (link: string, signedIn: boolean) => {
    setPhase('sending');
    let next = '/catalog';
    try {
      await api.post('/api/auth/link', { code: link });
      await reloadSession(queryClient);
    } catch (e) {
      if (!signedIn) {
        next = `/?loginError=${encodeURIComponent(isApiError(e) ? e.code : 'internal')}`;
      }
    }
    finish(next);
  });

  useEffect(() => {
    if (phase !== 'waiting' || status === 'loading' || held.current === null) return;
    // Send on its own only when `/me` definitely said "nobody is signed in" (401); any other
    // failure leaves the session state unknown, and a link must not replace a session unasked.
    if (status === 'signed-in' || sessionUnknown) {
      setPhase('confirm');
      return;
    }
    const link = held.current;
    held.current = null;
    void send(link, false);
  }, [phase, status, sessionUnknown]);

  if (phase === 'confirm') {
    return (
      <ConfirmCard
        hint={
          sessionUnknown
            ? strings.login.switchHintUnknown
            : strings.login.switchHint(me?.name ?? '')
        }
        onConfirm={() => {
          const link = held.current;
          held.current = null;
          if (link !== null) void send(link, !sessionUnknown);
        }}
        onCancel={() => {
          held.current = null;
          finish('/catalog');
        }}
      />
    );
  }
  if (phase !== 'idle' || (target !== null && !arrived)) {
    return (
      <div aria-busy="true" className="flex flex-col gap-3 p-6">
        <span className="sr-only">{strings.common.loading}</span>
        <Skeleton className="h-8 w-48" />
      </div>
    );
  }
  return <Outlet />;
}

function ConfirmCard({
  hint,
  onConfirm,
  onCancel,
}: {
  hint: string;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  useSystemTheme();
  return (
    <main className="flex min-h-svh items-center justify-center p-4">
      <Card className="w-full max-w-sm">
        <CardHeader>
          <CardTitle>
            <h1 className="text-xl font-semibold">{strings.login.switchTitle}</h1>
          </CardTitle>
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          <p className="text-sm text-muted-foreground">{hint}</p>
          <Button onClick={onConfirm}>{strings.login.switchConfirm}</Button>
          <Button variant="outline" onClick={onCancel}>
            {strings.login.switchCancel}
          </Button>
        </CardContent>
      </Card>
    </main>
  );
}
