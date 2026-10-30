import { screen, waitFor, within } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { describe, expect, it } from 'vite-plus/test';
import { server } from '../../test/msw/handlers.ts';
import { renderWithApp } from '../../test/setup.ts';
import { TokensPage } from './TokensPage.tsx';

describe('TokensPage', () => {
  it('shows the new token once, with the connect command, and forgets it on close', async () => {
    let tokens: unknown[] = [];
    server.use(
      http.get('/api/tokens', () => HttpResponse.json(tokens)),
      http.post('/api/tokens', async () => {
        tokens = [
          {
            id: 't1',
            name: 'laptop',
            prefix: 'sk_abcd',
            createdAt: '2026-10-30T10:00:00Z',
            lastUsedAt: null,
            revokedAt: null,
          },
        ];
        return HttpResponse.json(
          { id: 't1', prefix: 'sk_abcd', token: 'sk_abcdSECRETSECRET' },
          { status: 201 },
        );
      }),
    );
    const { user } = renderWithApp(<TokensPage />, { route: '/tokens' });
    await user.click(await screen.findByRole('button', { name: 'Create token' }));
    const dialog = await screen.findByRole('dialog');
    await user.type(within(dialog).getByLabelText('Name'), 'laptop');
    await user.click(within(dialog).getByRole('button', { name: 'Create' }));

    expect(await within(dialog).findByLabelText('Token')).toHaveProperty(
      'value',
      'sk_abcdSECRETSECRET',
    );
    expect(within(dialog).getByLabelText('Connect command')).toHaveProperty(
      'value',
      `claude mcp add --transport http mcp-registry ${window.location.origin}/mcp --header "Authorization: Bearer sk_abcdSECRETSECRET"`,
    );

    await user.click(within(dialog).getByRole('button', { name: 'Done' }));
    await waitFor(() => expect(screen.queryByDisplayValue('sk_abcdSECRETSECRET')).toBeNull());
    expect(await screen.findByText('sk_abcd…')).toBeTruthy();
    expect(document.body.textContent).not.toContain('SECRETSECRET');
  });
});
