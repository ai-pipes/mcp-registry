import { screen, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { StrictMode } from 'react';
import { Outlet } from 'react-router';
import { describe, expect, it } from 'vite-plus/test';
import { routes } from '@/app/routes.tsx';
import { errorResponse, server } from '../../test/msw/handlers.ts';
import { renderWithApp } from '../../test/setup.ts';

function StrictModeShell() {
  return (
    <StrictMode>
      <Outlet />
    </StrictMode>
  );
}

const signedOut = http.get('/api/auth/me', () => errorResponse(401, 'unauthenticated'));
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

  it('keeps a signed-in person in the app when the link is rejected', async () => {
    server.use(
      http.get('/api/auth/me', () =>
        HttpResponse.json({ userId: 'u1', name: 'Anna', role: 'member', groups: [] }),
      ),
      http.post('/api/auth/link', () => errorResponse(401, 'invalid_link')),
      catalog,
    );
    renderWithApp(routes, { route: '/login#link=bad' });
    expect(await screen.findByRole('heading', { name: 'Catalog', level: 1 })).toBeTruthy();
    await waitFor(() => expect(screen.queryByRole('alert')).toBeNull());
  });

  it('/login without a fragment goes to the catalog', async () => {
    server.use(signedOut);
    const { router } = renderWithApp(routes, { route: '/login' });
    await screen.findByRole('link', { name: 'Sign in with SSO' });
    expect(router.state.location.pathname).toBe('/catalog');
  });
});
