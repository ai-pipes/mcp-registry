import { screen, within } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { describe, expect, it } from 'vite-plus/test';
import { server } from '../../../test/msw/handlers.ts';
import { renderWithApp } from '../../../test/setup.ts';
import { AuditPage } from '../audit/AuditPage.tsx';
import { CallsPage } from './CallsPage.tsx';

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
