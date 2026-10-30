import { screen } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { describe, expect, it } from 'vite-plus/test';
import { errorResponse, server } from '../../test/msw/handlers.ts';
import { renderWithApp } from '../../test/setup.ts';
import { routes } from './routes.tsx';

const me = (role: 'admin' | 'member') =>
  http.get('/api/auth/me', () =>
    HttpResponse.json({ userId: 'u1', name: 'Anna', role, groups: [] }),
  );

describe('routes and access', () => {
  it('a signed-out visitor sees the SSO login with a returnTo link', async () => {
    server.use(http.get('/api/auth/me', () => errorResponse(401, 'unauthenticated')));
    renderWithApp(routes, { route: '/tokens' });
    const link = await screen.findByRole('link', { name: 'Sign in with SSO' });
    expect(link.getAttribute('href')).toBe('/api/auth/login?returnTo=%2Ftokens');
  });

  it.each([
    ['invalid_state', 'The sign-in attempt expired'],
    ['not_allowed', 'not in a group'],
    ['disabled', 'disabled by an administrator'],
    ['idp_unavailable', 'The sign-in provider is unavailable'],
    ['internal', 'Internal gateway error'],
    ['something_new', 'Sign-in failed'],
    ['__proto__', 'Sign-in failed'],
    ['constructor', 'Sign-in failed'],
    ['toString', 'Sign-in failed'],
  ])('/?loginError=%s shows a Russian message', async (code, text) => {
    server.use(http.get('/api/auth/me', () => errorResponse(401, 'unauthenticated')));
    renderWithApp(routes, { route: `/?loginError=${code}` });
    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain(text);
    const link = screen.getByRole('link', { name: 'Sign in with SSO' });
    expect(link.getAttribute('href')).toBe('/api/auth/login?returnTo=%2Fcatalog');
  });

  it('the login page follows the system dark theme', async () => {
    const original = window.matchMedia;
    window.matchMedia = (q: string) => {
      const list = original(q);
      return Object.assign(Object.create(list), { matches: true });
    };
    try {
      server.use(http.get('/api/auth/me', () => errorResponse(401, 'unauthenticated')));
      renderWithApp(routes, { route: '/' });
      await screen.findByRole('link', { name: 'Sign in with SSO' });
      expect(document.documentElement.classList.contains('dark')).toBe(true);
    } finally {
      window.matchMedia = original;
      document.documentElement.classList.remove('dark');
    }
  });

  it('a member sees no admin menu and "Access denied" on admin routes', async () => {
    server.use(me('member'));
    renderWithApp(routes, { route: '/admin/users' });
    expect(await screen.findByText('Access denied')).toBeTruthy();
    expect(screen.queryByRole('link', { name: 'Users' })).toBeNull();
    expect(screen.getByRole('link', { name: 'Tokens' })).toBeTruthy();
  });

  it('an admin sees the admin menu', async () => {
    server.use(
      me('admin'),
      http.get('/api/servers', () => HttpResponse.json([])),
    );
    renderWithApp(routes, { route: '/admin/servers' });
    expect(await screen.findByRole('link', { name: 'Users' })).toBeTruthy();
    expect(await screen.findByText('No servers yet')).toBeTruthy();
  });

  it('/ goes to the catalog', async () => {
    server.use(
      me('member'),
      http.get('/api/catalog', () => HttpResponse.json([])),
    );
    const { router } = renderWithApp(routes, { route: '/' });
    expect(await screen.findByText('The catalog is empty')).toBeTruthy();
    expect(router.state.location.pathname).toBe('/catalog');
  });
});
