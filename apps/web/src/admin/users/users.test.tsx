import { screen, waitFor, within } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { describe, expect, it } from 'vite-plus/test';
import { server } from '../../../test/msw/handlers.ts';
import { renderWithApp } from '../../../test/setup.ts';
import { UsersPage } from './UsersPage.tsx';

describe('UsersPage', () => {
  it('disables a user after confirmation', async () => {
    let body: unknown;
    server.use(
      http.get('/api/users', () =>
        HttpResponse.json([
          {
            id: 'u2',
            name: 'Boris',
            email: 'b@example.com',
            role: 'member',
            groups: ['mcp-users'],
            disabled: false,
            createdAt: '2026-10-01T10:00:00Z',
            lastLoginAt: null,
          },
        ]),
      ),
      http.post('/api/users/u2/disabled', async ({ request }) => {
        body = await request.json();
        return new HttpResponse(null, { status: 204 });
      }),
    );
    const { user } = renderWithApp(<UsersPage />, { route: '/admin/users' });
    await user.click(await screen.findByRole('button', { name: 'Disable' }));
    const dialog = await screen.findByRole('alertdialog');
    await user.click(within(dialog).getByRole('button', { name: 'Disable' }));
    await waitFor(() => expect(body).toEqual({ disabled: true }));
  });
});
