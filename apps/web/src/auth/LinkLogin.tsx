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
const ME_KEY = ['auth', 'me'] as const;

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
 * once (the code never stays in the URL or the history). Nothing is sent until the person
 * presses the confirm button, whatever the session state: a link must never sign anyone in
 * silently (login CSRF: someone tricking a signed-out person into opening a link for the
 * attacker's account). A declined link sends nothing; a rejected one leaves a session that
 * exists (also one made meanwhile in another tab) untouched.
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

  const send = useEffectEvent(async (link: string) => {
    setPhase('sending');
    let failure: unknown = null;
    try {
      await api.post('/api/auth/link', { code: link });
    } catch (e) {
      failure = e;
    }
    if (failure === null) {
      // A failed reload is not a sign-in error: the normal flow decides from `/catalog`.
      try {
        await reloadSession(queryClient);
      } catch {
        // ignored on purpose
      }
      finish('/catalog');
      return;
    }
    // Another tab may have signed in meanwhile: a session that exists now wins, silently.
    try {
      await queryClient.invalidateQueries({ queryKey: ME_KEY });
    } catch {
      // ignored on purpose: the cached state is used
    }
    if (queryClient.getQueryData(ME_KEY) != null) {
      finish('/catalog');
      return;
    }
    const error = isApiError(failure) ? failure.code : 'internal';
    finish(`/?loginError=${encodeURIComponent(error)}`);
  });

  useEffect(() => {
    if (phase !== 'waiting' || status === 'loading' || held.current === null) return;
    setPhase('confirm');
  }, [phase, status]);

  if (phase === 'confirm') {
    const signedOut = status === 'signed-out' && !sessionUnknown;
    return (
      <ConfirmCard
        title={signedOut ? strings.login.linkTitle : strings.login.switchTitle}
        hint={
          signedOut
            ? strings.login.linkHint
            : sessionUnknown
              ? strings.login.switchHintUnknown
              : strings.login.switchHint(me?.name ?? '')
        }
        cancelLabel={signedOut ? strings.login.linkCancel : strings.login.switchCancel}
        onConfirm={() => {
          const link = held.current;
          held.current = null;
          if (link !== null) void send(link);
        }}
        onCancel={() => {
          held.current = null;
          finish(signedOut ? '/' : '/catalog');
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
  title,
  hint,
  cancelLabel,
  onConfirm,
  onCancel,
}: {
  title: string;
  hint: string;
  cancelLabel: string;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  useSystemTheme();
  const safe = useRef<HTMLButtonElement>(null);
  // The safe choice gets the focus, so a stray Enter cannot sign anyone in.
  useEffect(() => {
    safe.current?.focus();
  }, []);
  return (
    <main className="flex min-h-svh items-center justify-center p-4">
      <Card className="w-full max-w-sm">
        <CardHeader>
          <CardTitle>
            <h1 className="text-xl font-semibold">{title}</h1>
          </CardTitle>
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          <p className="text-sm text-muted-foreground">{hint}</p>
          <Button onClick={onConfirm}>{strings.login.switchConfirm}</Button>
          <Button ref={safe} variant="outline" onClick={onCancel}>
            {cancelLabel}
          </Button>
        </CardContent>
      </Card>
    </main>
  );
}
