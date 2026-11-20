import { screen, waitFor, within } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { describe, expect, it } from 'vite-plus/test';
import { server } from '../../../test/msw/handlers.ts';
import { renderWithApp } from '../../../test/setup.ts';
import { AuditPage } from '../audit/AuditPage.tsx';
import { CallsPage } from './CallsPage.tsx';

const row = (n: number) => ({
  id: String(1000 - n),
  userId: 'u1',
  tokenId: null,
  server: 'gitea',
  tool: `tool_${n}`,
  startedAt: '2026-10-30T10:00:00.000Z',
  durationMs: 1,
  outcome: 'ok',
});

describe('CallsPage and AuditPage', () => {
  it('the call log shows the user by name and the outcome in Russian', async () => {
    server.use(
      http.get('/api/servers', () => HttpResponse.json([])),
      http.get('/api/users', () => HttpResponse.json([])),
      http.get('/api/calls', () =>
        HttpResponse.json([
          {
            id: '1',
            userId: 'u1',
            tokenId: null,
            server: 'gitea',
            tool: 'create_issue',
            startedAt: '2026-10-30T10:00:00Z',
            durationMs: 42,
            outcome: 'denied:quota',
          },
        ]),
      ),
    );
    renderWithApp(<CallsPage />, { route: '/admin/calls' });
    expect(await screen.findByText('create_issue')).toBeTruthy();
    const table = within(screen.getByRole('table'));
    expect(table.getByText('Denied: quota')).toBeTruthy();
    expect(table.getByText('42 ms')).toBeTruthy();
  });

  it('the outcome filter sends the chosen outcome and "All" clears it', async () => {
    const seen: (string | null)[] = [];
    server.use(
      http.get('/api/servers', () => HttpResponse.json([])),
      http.get('/api/users', () => HttpResponse.json([])),
      http.get('/api/calls', ({ request }) => {
        seen.push(new URL(request.url).searchParams.get('outcome'));
        return HttpResponse.json([]);
      }),
    );
    const { user } = renderWithApp(<CallsPage />, { route: '/admin/calls' });
    await user.click(await screen.findByRole('combobox', { name: 'Outcome' }));
    await user.click(await screen.findByRole('option', { name: 'Denied: quota' }));
    await waitFor(() => expect(seen.at(-1)).toBe('denied:quota'));
    await user.click(screen.getByRole('combobox', { name: 'Outcome' }));
    await user.click(await screen.findByRole('option', { name: 'All' }));
    await waitFor(() => expect(seen.at(-1)).toBeNull());
  });

  it('the second page request carries both the time and the id of the last row', async () => {
    const queries: URLSearchParams[] = [];
    server.use(
      http.get('/api/servers', () => HttpResponse.json([])),
      http.get('/api/users', () => HttpResponse.json([])),
      http.get('/api/calls', ({ request }) => {
        const q = new URL(request.url).searchParams;
        queries.push(q);
        return HttpResponse.json(
          q.has('before') ? [] : Array.from({ length: 100 }, (_, n) => row(n)),
        );
      }),
    );
    const { user } = renderWithApp(<CallsPage />, { route: '/admin/calls' });
    await user.click(await screen.findByRole('button', { name: 'Show more' }));
    await waitFor(() => expect(queries).toHaveLength(2));
    expect(queries[1]?.get('before')).toBe('2026-10-30T10:00:00.000Z');
    expect(queries[1]?.get('beforeId')).toBe('901');
  });

  it('the audit log lists actions', async () => {
    server.use(
      http.get('/api/users', () => HttpResponse.json([])),
      http.get('/api/audit', () =>
        HttpResponse.json([
          {
            id: '5',
            at: '2026-10-30T10:00:00Z',
            actorId: null,
            action: 'server.created',
            target: 'gitea',
            meta: {},
          },
        ]),
      ),
    );
    renderWithApp(<AuditPage />, { route: '/admin/audit' });
    expect(await screen.findByText('server.created')).toBeTruthy();
    expect(screen.getByText('system')).toBeTruthy();
  });
});
