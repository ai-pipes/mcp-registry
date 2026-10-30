import { screen, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { describe, expect, it, vi } from 'vite-plus/test';
import { redirectTo } from '@/lib/navigate.ts';
import { errorResponse, server } from '../../test/msw/handlers.ts';
import { renderWithApp } from '../../test/setup.ts';
import { ConnectionsPage } from './ConnectionsPage.tsx';

vi.mock('@/lib/navigate.ts', () => ({ redirectTo: vi.fn() }));

const list = [
  { server: 'gitea', name: 'Gitea', authKind: 'oauth_user', status: 'missing' },
  { server: 'youtrack', name: 'YouTrack', authKind: 'oauth_user', status: 'ready' },
  { server: 'old', name: 'Old', authKind: 'oauth_user', status: 'expired' },
  { server: 'grafana', name: 'Grafana', authKind: 'pat_user', status: 'missing' },
];

function useList() {
  server.use(http.get('/api/connections', () => HttpResponse.json(list)));
}

describe('ConnectionsPage', () => {
  it('shows a status in Russian for every connection', async () => {
    useList();
    renderWithApp(<ConnectionsPage />, { route: '/connections' });
    expect(await screen.findByText('Connected')).toBeTruthy();
    expect(screen.getByText('Expired')).toBeTruthy();
    expect(screen.getAllByText('Not connected')).toHaveLength(2);
  });

  it('"Connect" starts the flow and sends the browser to the returned url', async () => {
    useList();
    let called = false;
    server.use(
      http.post('/api/connections/gitea/start', () => {
        called = true;
        return HttpResponse.json({ url: 'https://gitea.example.com/login/oauth/authorize?x=1' });
      }),
    );
    const { user } = renderWithApp(<ConnectionsPage />, { route: '/connections' });
    await user.click(await screen.findByRole('button', { name: 'Connect Gitea' }));
    await waitFor(() =>
      expect(redirectTo).toHaveBeenCalledWith(
        'https://gitea.example.com/login/oauth/authorize?x=1',
      ),
    );
    expect(called).toBe(true);
  });

  it('?connected= shows a success toast and cleans the query string', async () => {
    useList();
    const { router } = renderWithApp(<ConnectionsPage />, {
      route: '/connections?connected=gitea',
    });
    expect(await screen.findByText('Gitea connected')).toBeTruthy();
    await waitFor(() => expect(router.state.location.search).toBe(''));
  });

  it('?error= shows an ErrorAlert with a Russian text and cleans the query string', async () => {
    useList();
    const { router } = renderWithApp(<ConnectionsPage />, { route: '/connections?error=denied' });
    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('You declined');
    await waitFor(() => expect(router.state.location.search).toBe(''));
    expect(screen.getByRole('alert')).toBeTruthy();
  });

  it('?error=internal tells it is the gateway, not the provider', async () => {
    useList();
    renderWithApp(<ConnectionsPage />, { route: '/connections?error=internal' });
    expect((await screen.findByRole('alert')).textContent).toContain('Internal gateway error');
  });

  it.each(['__proto__', 'constructor'])(
    '?error=%s falls back to a generic message',
    async (code) => {
      useList();
      renderWithApp(<ConnectionsPage />, { route: `/connections?error=${code}` });
      expect((await screen.findByRole('alert')).textContent).toContain(
        'The provider did not accept',
      );
    },
  );

  it('?connected= is cleaned even when the list request fails', async () => {
    server.use(http.get('/api/connections', () => errorResponse(500, 'boom')));
    const { router } = renderWithApp(<ConnectionsPage />, {
      route: '/connections?connected=gitea',
    });
    await waitFor(() => expect(router.state.location.search).toBe(''));
  });

  it('a start failure goes to an ErrorAlert', async () => {
    useList();
    server.use(
      http.post('/api/connections/gitea/start', () => errorResponse(400, 'invalid_auth_kind')),
    );
    const { user } = renderWithApp(<ConnectionsPage />, { route: '/connections' });
    await user.click(await screen.findByRole('button', { name: 'Connect Gitea' }));
    expect(await screen.findByRole('alert')).toBeTruthy();
    expect(redirectTo).not.toHaveBeenCalledWith(expect.stringContaining('undefined'));
  });

  it('a pat_user server takes a token, PUTs it and forgets it', async () => {
    useList();
    let body: unknown;
    server.use(
      http.put('/api/connections/grafana/pat', async ({ request }) => {
        body = await request.json();
        return new HttpResponse(null, { status: 204 });
      }),
    );
    const { user } = renderWithApp(<ConnectionsPage />, { route: '/connections' });
    const input = await screen.findByLabelText('Token for Grafana');
    await user.type(input, 'glsa_secret');
    await user.click(screen.getByRole('button', { name: 'Save token for Grafana' }));
    await waitFor(() => expect(body).toEqual({ token: 'glsa_secret' }));
    expect(await screen.findByText('Token for Grafana saved')).toBeTruthy();
    expect(screen.getByLabelText('Token for Grafana')).toHaveProperty('value', '');
  });
});
