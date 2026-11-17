import { screen } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { describe, expect, it } from 'vite-plus/test';
import { server } from '../../test/msw/handlers.ts';
import { renderWithApp } from '../../test/setup.ts';
import { LoginPage } from './LoginPage.tsx';

describe('LoginPage', () => {
  it('without OIDC shows the command and no SSO button', async () => {
    server.use(http.get('/api/auth/methods', () => HttpResponse.json({ oidc: false })));
    renderWithApp(<LoginPage returnTo="/catalog" />);
    const hint = await screen.findByText(/Single sign-on is not configured\. Sign in with/);
    expect(hint.textContent).toBe(
      'Single sign-on is not configured. Sign in with a one-time link created on the server:',
    );
    const command = screen.getByText('node dist/cli.js login-link <sub>');
    expect(command.tagName).toBe('CODE');
    expect(screen.queryByRole('link', { name: 'Sign in with SSO' })).toBeNull();
  });

  it('with OIDC is unchanged', async () => {
    renderWithApp(<LoginPage returnTo="/tokens" />);
    const link = await screen.findByRole('link', { name: 'Sign in with SSO' });
    expect(link.getAttribute('href')).toBe('/api/auth/login?returnTo=%2Ftokens');
    expect(screen.queryByText('node dist/cli.js login-link <sub>')).toBeNull();
  });

  it('shows the SSO button when the methods request fails', async () => {
    server.use(
      http.get('/api/auth/methods', () => HttpResponse.json({ code: 'internal' }, { status: 500 })),
    );
    renderWithApp(<LoginPage returnTo="/catalog" />);
    expect(
      await screen.findByRole('link', { name: 'Sign in with SSO' }, { timeout: 500 }),
    ).toBeTruthy();
  });

  it('shows the oidc_not_configured error text', async () => {
    renderWithApp(<LoginPage returnTo="/" />, { route: '/?loginError=oidc_not_configured' });
    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('Single sign-on is not configured on this server.');
  });
});
