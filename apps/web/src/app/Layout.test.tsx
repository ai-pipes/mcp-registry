import { screen } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { describe, expect, it } from 'vite-plus/test';
import { errorResponse, server } from '../../test/msw/handlers.ts';
import { renderWithApp } from '../../test/setup.ts';
import { routes } from './routes.tsx';

const signedIn = () =>
  server.use(
    http.get('/api/auth/me', () =>
      HttpResponse.json({ userId: 'u1', name: 'Anna', role: 'member', groups: [] }),
    ),
  );

describe('sidebar version', () => {
  it('shows the gateway version in the sidebar', async () => {
    signedIn();
    server.use(http.get('/api/version', () => HttpResponse.json({ version: '1.2.3' })));
    renderWithApp(routes, { route: '/catalog' });
    expect(await screen.findByText('v1.2.3')).toBeTruthy();
  });

  it('shows nothing when the version request fails', async () => {
    signedIn();
    server.use(http.get('/api/version', () => errorResponse(500, 'internal')));
    renderWithApp(routes, { route: '/catalog' });
    expect(await screen.findByText('Anna')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Sign out' })).toBeTruthy();
    expect(screen.queryByText(/^v\d/)).toBeNull();
  });

  it('a long version does not add inline styles', async () => {
    signedIn();
    server.use(http.get('/api/version', () => HttpResponse.json({ version: '1.10.12-rc.1' })));
    renderWithApp(routes, { route: '/catalog' });
    const line = await screen.findByText('v1.10.12-rc.1');
    expect(line.hasAttribute('style')).toBe(false);
    expect(document.querySelectorAll('style').length).toBe(0);
  });
});
