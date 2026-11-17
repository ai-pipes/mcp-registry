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
  it('signs in from a link and lands on the catalog', async () => {
    let session = false;
    const bodies: unknown[] = [];
    server.use(
      http.get('/api/auth/me', () =>
        session
          ? HttpResponse.json({ userId: 'u1', name: 'Anna', role: 'member', groups: [] })
          : errorResponse(401, 'unauthenticated'),
      ),
      http.post('/api/auth/link', async ({ request }) => {
        bodies.push(await request.json());
        session = true;
        return new HttpResponse(null, { status: 204 });
      }),
      catalog,
    );
    const { router } = renderWithApp(routes, { route: '/login#link=ll_ok' });
    expect(await screen.findByRole('heading', { name: 'Catalog', level: 1 })).toBeTruthy();
    expect(bodies).toEqual([{ code: 'll_ok' }]);
    expect(router.state.location.hash).toBe('');
    expect(router.state.location.pathname).toBe('/catalog');
  });

  it('sends the code exactly once', async () => {
    let calls = 0;
    server.use(
      signedOut,
      http.post('/api/auth/link', () => {
        calls += 1;
        return new HttpResponse(null, { status: 204 });
      }),
      catalog,
    );
    const { router } = renderWithApp([{ element: <StrictModeShell />, children: routes }], {
      route: '/login#link=ll_ok',
    });
    await waitFor(() => expect(router.state.location.pathname).toBe('/catalog'));
    expect(calls).toBe(1);
  });

  it('shows invalid_link on the login page when the link is rejected and nobody is signed in', async () => {
    server.use(
      signedOut,
      http.post('/api/auth/link', () => errorResponse(401, 'invalid_link')),
    );
    const { router } = renderWithApp(routes, { route: '/login#link=bad' });
    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('invalid, expired or already used');
    expect(router.state.location.hash).toBe('');
  });

  describe('with a session', () => {
    const bodies: unknown[] = [];
    function signedIn(linkStatus: number) {
      bodies.length = 0;
      server.use(
        http.get('/api/auth/me', () =>
          HttpResponse.json({ userId: 'u1', name: 'Anna', role: 'member', groups: [] }),
        ),
        http.post('/api/auth/link', async ({ request }) => {
          bodies.push(await request.json());
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
      expect(bodies).toEqual([]);
    });

    it('asks before replacing an existing session', async () => {
      signedIn(204);
      const { router } = renderWithApp(routes, { route: '/login#link=ll_x' });
      expect(await screen.findByRole('heading', { name: 'Sign in with this link?' })).toBeTruthy();
      expect(screen.getByText(/You are signed in as Anna\./)).toBeTruthy();
      await new Promise((r) => setTimeout(r, 300)); // a late send would show up here
      expect(screen.getByRole('heading', { name: 'Sign in with this link?' })).toBeTruthy();
      expect(bodies).toEqual([]);
      expect(router.state.location.hash).toBe('');
    });

    it('replaces the session only after confirmation', async () => {
      signedIn(204);
      const { user } = renderWithApp(routes, { route: '/login#link=ll_x' });
      await user.click(await screen.findByRole('button', { name: 'Sign in with the link' }));
      await catalogHeading();
      expect(bodies).toEqual([{ code: 'll_x' }]);
    });

    it('stays signed in when the person declines', async () => {
      signedIn(204);
      const { user } = renderWithApp(routes, { route: '/login#link=ll_x' });
      await user.click(await screen.findByRole('button', { name: 'Stay signed in' }));
      await catalogHeading();
      expect(bodies).toEqual([]);
    });

    it('stays signed in when a confirmed link is rejected', async () => {
      signedIn(401);
      const { user } = renderWithApp(routes, { route: '/login#link=ll_x' });
      await user.click(await screen.findByRole('button', { name: 'Sign in with the link' }));
      await catalogHeading();
      expect(bodies).toEqual([{ code: 'll_x' }]);
      expect(screen.queryByRole('alert')).toBeNull();
    });
  });

  describe('when the session check fails', () => {
    const bodies: unknown[] = [];
    beforeEach(() => {
      bodies.length = 0;
      server.use(
        http.get('/api/auth/me', () => errorResponse(500, 'internal')),
        http.post('/api/auth/link', async ({ request }) => {
          bodies.push(await request.json());
          return new HttpResponse(null, { status: 204 });
        }),
        catalog,
      );
    });

    it('does not send the code when the session check fails', async () => {
      renderWithApp(routes, { route: '/login#link=ll_x' });
      expect(await screen.findByText(strings.login.switchHintUnknown)).toBeTruthy();
      await new Promise((r) => setTimeout(r, 300));
      expect(bodies).toEqual([]);
    });

    it('sends after confirmation when the session check failed', async () => {
      const { user } = renderWithApp(routes, { route: '/login#link=ll_x' });
      await user.click(await screen.findByRole('button', { name: 'Sign in with the link' }));
      await waitFor(() => expect(bodies).toEqual([{ code: 'll_x' }]));
    });
  });

  it('sends the code exactly once when the session is already known at mount', async () => {
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
    const { router } = renderWithApp(
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
