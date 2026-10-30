import { screen, waitFor, within } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { describe, expect, it } from 'vite-plus/test';
import { serverView } from '../../../test/fixtures.ts';
import { server } from '../../../test/msw/handlers.ts';
import { renderWithApp } from '../../../test/setup.ts';
import { ServerPage } from './ServerPage.tsx';

const tool = (name: string, hidden = false) => ({
  ref: `gitea.${name}`,
  name,
  title: null,
  description: `Does ${name}`,
  annotations: {},
  inputSchema: {},
  outputSchema: null,
  hidden,
});

function mount(view = serverView()) {
  server.use(
    http.get('/api/servers/gitea', () => HttpResponse.json(view)),
    http.get('/api/servers/gitea/tools', () =>
      HttpResponse.json([tool('create_issue'), tool('list_repos', true)]),
    ),
  );
  return renderWithApp(<ServerPage />, {
    route: '/admin/servers/gitea',
    path: '/admin/servers/:slug',
  });
}

describe('ServerPage', () => {
  it('"Sync now" toasts the counts', async () => {
    server.use(
      http.post('/api/servers/gitea/sync', () =>
        HttpResponse.json({ added: 3, changed: 1, removed: 2 }),
      ),
    );
    const { user } = mount();
    await user.click(await screen.findByRole('button', { name: 'Sync now' }));
    expect(await screen.findByText('Added 3, changed 1, removed 2')).toBeTruthy();
  });

  it('shows lastSyncError in an ErrorAlert', async () => {
    mount(serverView({ lastSyncError: 'OAuth provider unavailable' }));
    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('OAuth provider unavailable');
  });

  it('a sync failure from REST goes to an ErrorAlert with its Russian text', async () => {
    server.use(
      http.post('/api/servers/gitea/sync', () =>
        HttpResponse.json(
          { error: { code: 'needs_connection', message: 'Admin connection required' } },
          { status: 400 },
        ),
      ),
    );
    const { user } = mount();
    await user.click(await screen.findByRole('button', { name: 'Sync now' }));
    expect((await screen.findByRole('alert')).textContent).toContain('Admin connection required');
  });

  it('the "Hidden" switch of a tool PUTs …/hidden', async () => {
    let body: unknown;
    server.use(
      http.put('/api/servers/gitea/tools/create_issue/hidden', async ({ request }) => {
        body = await request.json();
        return new HttpResponse(null, { status: 204 });
      }),
    );
    const { user } = mount();
    const row = (await screen.findByText('create_issue')).closest('tr')!;
    await user.click(within(row).getByRole('checkbox', { name: 'Hidden: create_issue' }));
    await waitFor(() => expect(body).toEqual({ hidden: true }));
  });
});
