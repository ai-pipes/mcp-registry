import { screen, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { StrictMode } from 'react';
import { Outlet } from 'react-router';
import { beforeEach, describe, expect, it } from 'vite-plus/test';
import { AuthProvider, useAuth } from './AuthProvider.tsx';
import { LinkLogin } from './LinkLogin.tsx';
import { routes } from '@/app/routes.tsx';
import { errorResponse, server } from '../../test/msw/handlers.ts';
import { strings } from '@/strings.ts';
import { renderWithApp } from '../../test/setup.ts';

function StrictModeShell() {
  return (
    <StrictMode>
      <Outlet />
    </StrictMode>
  );
}

function AfterSession() {
  const { status } = useAuth();
  return status === 'loading' ? null : <LinkLogin />;
}

const signedOut = http.get('/api/auth/me', () => errorResponse(401, 'unauthenticated'));
const catalogHeading = () => screen.findByRole('heading', { name: 'Catalog', level: 1 });
const catalog = http.get('/api/catalog', () => HttpResponse.json([]));

describe('LinkLogin', () => {
  const bodies: unknown[] = [];
  function signedOutWithLink(
    linkResponse: () => Response = () => new HttpResponse(null, { status: 204 }),
  ) {
    bodies.length = 0;
    let session = false;
    server.use(
      http.get('/api/auth/me', () =>
        session
          ? HttpResponse.json({ userId: 'u1', name: 'Anna', role: 'member', groups: [] })
          : errorResponse(401, 'unauthenticated'),
      ),
      http.post('/api/auth/link', async ({ request }) => {
        bodies.push(await request.json());
        const res = linkResponse();
        if (res.status === 204) session = true;
        return res;
      }),
      catalog,
    );
    return {
      appear: () => {
        session = true;
      },
    };
  }

  it('asks before signing in when nobody is signed in', async () => {
    signedOutWithLink();
    const { router } = renderWithApp(routes, { route: '/login#link=ll_x' });
    expect(await screen.findByText(strings.login.linkHint)).toBeTruthy();
    expect(screen.getByRole('heading', { name: strings.login.linkTitle })).toBeTruthy();
    await new Promise((r) => setTimeout(r, 300)); // a late send would show up here
    expect(bodies).toEqual([]);
    expect(router.state.location.hash).toBe('');
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Cancel' }));
  });

  it('signs in after confirmation and lands on the catalog', async () => {
    signedOutWithLink();
    const { user, router } = renderWithApp(routes, { route: '/login#link=ll_x' });
    await user.click(await screen.findByRole('button', { name: 'Sign in with the link' }));
    expect(await catalogHeading()).toBeTruthy();
    expect(bodies).toEqual([{ code: 'll_x' }]);
    expect(router.state.location.pathname).toBe('/catalog');
  });

  it('sends exactly one POST after one confirmation under StrictMode', async () => {
    signedOutWithLink();
    const { user } = renderWithApp([{ element: <StrictModeShell />, children: routes }], {
      route: '/login#link=ll_x',
    });
    await user.click(await screen.findByRole('button', { name: 'Sign in with the link' }));
    await catalogHeading();
    await new Promise((r) => setTimeout(r, 200));
    expect(bodies).toEqual([{ code: 'll_x' }]);
  });

  it('cancel returns to the login page without sending', async () => {
    signedOutWithLink();
    const { user } = renderWithApp(routes, { route: '/login#link=ll_x' });
    await user.click(await screen.findByRole('button', { name: 'Cancel' }));
    expect(await screen.findByRole('link', { name: 'Sign in with SSO' })).toBeTruthy();
    expect(bodies).toEqual([]);
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('a rejected link shows invalid_link when nobody is signed in', async () => {
    signedOutWithLink(() => errorResponse(401, 'invalid_link'));
    const { user, router } = renderWithApp(routes, { route: '/login#link=bad' });
    await user.click(await screen.findByRole('button', { name: 'Sign in with the link' }));
    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('invalid, expired or already used');
    expect(router.state.location.hash).toBe('');
  });

  it('a rejected link lands in the app when a session appeared meanwhile', async () => {
    const ctl = signedOutWithLink(() => errorResponse(401, 'invalid_link'));
    const { user } = renderWithApp(routes, { route: '/login#link=ll_x' });
    const confirm = await screen.findByRole('button', { name: 'Sign in with the link' });
    ctl.appear(); // another tab signed in
    await user.click(confirm);
    await catalogHeading();
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('shows rate_limited text when the server answers 429', async () => {
    signedOutWithLink(() => errorResponse(429, 'rate_limited'));
    const { user } = renderWithApp(routes, { route: '/login#link=ll_x' });
    await user.click(await screen.findByRole('button', { name: 'Sign in with the link' }));
    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('Too many failed sign-in attempts');
  });

  describe('with a session', () => {
    const sessionBodies: unknown[] = [];
    function signedIn(linkStatus: number) {
      sessionBodies.length = 0;
      server.use(
        http.get('/api/auth/me', () =>
          HttpResponse.json({ userId: 'u1', name: 'Anna', role: 'member', groups: [] }),
        ),
        http.post('/api/auth/link', async ({ request }) => {
          sessionBodies.push(await request.json());
          return linkStatus === 204
            ? new HttpResponse(null, { status: 204 })
            : errorResponse(linkStatus, 'invalid_link');
        }),
        catalog,
      );
    }
    it('ignores a link fragment outside /login', async () => {
      signedIn(204);
      renderWithApp(routes, { route: '/catalog#link=ll_x' });
      await catalogHeading();
      expect(sessionBodies).toEqual([]);
    });

    it('asks before replacing an existing session', async () => {
      signedIn(204);
      const { router } = renderWithApp(routes, { route: '/login#link=ll_x' });
      expect(await screen.findByRole('heading', { name: 'Sign in with this link?' })).toBeTruthy();
      expect(screen.getByText(/You are signed in as Anna\./)).toBeTruthy();
      await new Promise((r) => setTimeout(r, 300)); // a late send would show up here
      expect(screen.getByRole('heading', { name: 'Sign in with this link?' })).toBeTruthy();
      expect(sessionBodies).toEqual([]);
      expect(router.state.location.hash).toBe('');
    });

    it('replaces the session only after confirmation', async () => {
      signedIn(204);
      const { user } = renderWithApp(routes, { route: '/login#link=ll_x' });
      await user.click(await screen.findByRole('button', { name: 'Sign in with the link' }));
      await catalogHeading();
      expect(sessionBodies).toEqual([{ code: 'll_x' }]);
    });

    it('stays signed in when the person declines', async () => {
      signedIn(204);
      const { user } = renderWithApp(routes, { route: '/login#link=ll_x' });
      await user.click(await screen.findByRole('button', { name: 'Stay signed in' }));
      await catalogHeading();
      expect(sessionBodies).toEqual([]);
    });

    it('stays signed in when a confirmed link is rejected', async () => {
      signedIn(401);
      const { user } = renderWithApp(routes, { route: '/login#link=ll_x' });
      await user.click(await screen.findByRole('button', { name: 'Sign in with the link' }));
      await catalogHeading();
      expect(sessionBodies).toEqual([{ code: 'll_x' }]);
      expect(screen.queryByRole('alert')).toBeNull();
    });
  });

  describe('when the session check fails', () => {
    const sessionBodies: unknown[] = [];
    beforeEach(() => {
      sessionBodies.length = 0;
      server.use(
        http.get('/api/auth/me', () => errorResponse(500, 'internal')),
        http.post('/api/auth/link', async ({ request }) => {
          sessionBodies.push(await request.json());
          return new HttpResponse(null, { status: 204 });
        }),
        catalog,
      );
    });

    it('does not send the code when the session check fails', async () => {
      renderWithApp(routes, { route: '/login#link=ll_x' });
      expect(await screen.findByText(strings.login.switchHintUnknown)).toBeTruthy();
      await new Promise((r) => setTimeout(r, 300));
      expect(sessionBodies).toEqual([]);
    });

    it('sends after confirmation when the session check failed', async () => {
      const { user } = renderWithApp(routes, { route: '/login#link=ll_x' });
      await user.click(await screen.findByRole('button', { name: 'Sign in with the link' }));
      await waitFor(() => expect(sessionBodies).toEqual([{ code: 'll_x' }]));
    });
  });

  it('renders the card once and sends one POST after one click when the session is known at mount', async () => {
    let calls = 0;
    server.use(
      signedOut,
      http.post('/api/auth/link', () => {
        calls += 1;
        return new HttpResponse(null, { status: 204 });
      }),
      catalog,
    );
    // `LinkLogin` mounts only once `/me` has settled, so `status` is never `loading` for it.
    const { router, user } = renderWithApp(
      [
        {
          element: <StrictModeShell />,
          children: [
            {
              element: (
                <AuthProvider>
                  <AfterSession />
                </AuthProvider>
              ),
              children: [
                { path: 'login', element: <p>login</p> },
                { path: 'catalog', element: <p>catalog</p> },
              ],
            },
          ],
        },
      ],
      { route: '/login#link=ll_x' },
    );
    await new Promise((r) => setTimeout(r, 200));
    expect(calls).toBe(0);
    expect(screen.getAllByRole('button', { name: 'Sign in with the link' })).toHaveLength(1);
    await user.click(screen.getByRole('button', { name: 'Sign in with the link' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/catalog'));
    await new Promise((r) => setTimeout(r, 200));
    expect(calls).toBe(1);
  });

  it('/login without a fragment goes to the catalog', async () => {
    server.use(signedOut);
    const { router } = renderWithApp(routes, { route: '/login' });
    await screen.findByRole('link', { name: 'Sign in with SSO' });
    expect(router.state.location.pathname).toBe('/catalog');
  });
});
