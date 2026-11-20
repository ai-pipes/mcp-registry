import { screen, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { describe, expect, it } from 'vite-plus/test';
import { serverView } from '../../../test/fixtures.ts';
import { errorResponse, server } from '../../../test/msw/handlers.ts';
import { renderWithApp } from '../../../test/setup.ts';
import { ServerForm } from './ServerForm.tsx';

const noop = () => {};

const sharedView = () =>
  serverView({
    auth: { kind: 'shared_secret', header: 'Authorization', template: 'Bearer {secret}' },
    secretSet: true,
  });
const oauthView = () =>
  serverView({
    auth: {
      kind: 'oauth_user',
      header: 'Authorization',
      template: 'Bearer {secret}',
      oauth: {
        authorizeUrl: 'https://idp.example/authorize',
        tokenUrl: 'https://idp.example/token',
        clientId: 'cid',
        scopes: ['read'],
      },
    },
    secretSet: true,
  });

describe('ServerForm', () => {
  it('auth kind decides the fields: oauth vs shared/pat vs none', async () => {
    const { user } = renderWithApp(<ServerForm onSaved={noop} />);
    expect(screen.queryByLabelText('Header')).toBeNull();

    await user.click(screen.getByRole('radio', { name: 'Shared secret' }));
    expect(screen.getByLabelText('Header')).toBeTruthy();
    expect(screen.getByLabelText('Value template')).toBeTruthy();
    expect(screen.getByLabelText('Secret value')).toBeTruthy();
    expect(screen.queryByLabelText('Authorize URL')).toBeNull();

    await user.click(screen.getByRole('radio', { name: 'User token (PAT)' }));
    expect(screen.getByLabelText('Header')).toBeTruthy();
    expect(screen.queryByLabelText('Secret value')).toBeNull();
    expect(screen.queryByLabelText('Client secret')).toBeNull();

    await user.click(screen.getByRole('radio', { name: 'User OAuth' }));
    for (const label of ['Authorize URL', 'Token URL', 'Client ID', 'Client secret', 'Scopes']) {
      expect(screen.getByLabelText(label)).toBeTruthy();
    }
    expect(screen.getByLabelText('Header')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Add parameter' })).toBeTruthy();
  });

  it('with secretSet the secret shows "set" and an empty input', () => {
    renderWithApp(
      <ServerForm
        server={serverView({
          auth: { kind: 'shared_secret', header: 'Authorization', template: 'Bearer {secret}' },
          secretSet: true,
        })}
        onSaved={noop}
      />,
    );
    expect(screen.getByText('set')).toBeTruthy();
    expect(screen.getByLabelText('Secret value')).toHaveProperty('value', '');
  });

  it('warns that changing the auth kind clears the stored secret', async () => {
    const { user } = renderWithApp(
      <ServerForm
        server={serverView({
          auth: { kind: 'shared_secret', header: 'Authorization', template: 'Bearer {secret}' },
          secretSet: true,
        })}
        onSaved={noop}
      />,
    );
    expect(screen.queryByText(/resets the stored secret/)).toBeNull();
    await user.click(screen.getByRole('radio', { name: 'No authorization' }));
    expect(screen.getByText(/resets the stored secret/)).toBeTruthy();
  });

  it('warns before saving that changing the URL of a shared-secret server clears the secret', async () => {
    const { user } = renderWithApp(<ServerForm server={sharedView()} onSaved={noop} />);
    expect(screen.queryByText(/clears the stored secret/)).toBeNull();
    const url = screen.getByLabelText('URL');
    await user.clear(url);
    await user.type(url, 'https://new.example/mcp');
    expect(screen.getByText(/Changing the URL clears the stored secret/)).toBeTruthy();
    // typing the secret again in the same save keeps it: no warning then
    await user.type(screen.getByLabelText('Secret value'), 'again');
    expect(screen.queryByText(/clears the stored secret/)).toBeNull();
  });

  it('warns for the token URL, revoke URL or client ID of an OAuth server, not for other fields', async () => {
    const { user } = renderWithApp(<ServerForm server={oauthView()} onSaved={noop} />);
    await user.type(screen.getByLabelText('Name'), ' 2');
    await user.type(screen.getByLabelText('Authorize URL'), '2');
    await user.type(screen.getByLabelText('URL'), '2');
    expect(screen.queryByText(/clears the stored client secret/)).toBeNull();
    await user.type(screen.getByLabelText('Client ID'), '2');
    expect(screen.getByText(/clears the stored client secret/)).toBeTruthy();
  });

  it('after saving a change that cleared the secret it says the secret must be entered again', async () => {
    let saved = false;
    server.use(
      http.put('/api/servers/gitea', () => {
        saved = true;
        return HttpResponse.json(
          serverView({ ...sharedView(), url: 'https://new.example/mcp', secretSet: false }),
        );
      }),
    );
    const { user } = renderWithApp(<ServerForm server={sharedView()} onSaved={noop} />);
    const url = screen.getByLabelText('URL');
    await user.clear(url);
    await user.type(url, 'https://new.example/mcp');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(saved).toBe(true));
    expect(await screen.findByText(/The stored secret was cleared/)).toBeTruthy();
  });

  it('a secret entered with the address change is saved and no re-entry notice shows', async () => {
    server.use(
      http.put('/api/servers/gitea', () =>
        HttpResponse.json(serverView({ ...sharedView(), secretSet: false })),
      ),
      http.put('/api/servers/gitea/secret', () => new HttpResponse(null, { status: 204 })),
    );
    const { user } = renderWithApp(<ServerForm server={sharedView()} onSaved={noop} />);
    const url = screen.getByLabelText('URL');
    await user.clear(url);
    await user.type(url, 'https://new.example/mcp');
    await user.type(screen.getByLabelText('Secret value'), 'again');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await screen.findByText('Server saved');
    expect(screen.queryByText(/The stored secret was cleared/)).toBeNull();
  });

  it('client-side schema errors appear under the field', async () => {
    const { user } = renderWithApp(<ServerForm onSaved={noop} />);
    await user.type(screen.getByLabelText('Slug'), 'gitea');
    await user.type(screen.getByLabelText('Name'), 'Gitea');
    await user.type(screen.getByLabelText('URL'), 'not-a-url');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    const field = screen.getByLabelText('URL');
    expect(field.getAttribute('aria-invalid')).toBe('true');
    expect(
      document.getElementById(field.getAttribute('aria-describedby')?.split(' ')[0] ?? '')
        ?.textContent,
    ).toBeTruthy();
  });

  it('creates an oauth server with scopes and extra params, secret in the body', async () => {
    let body: unknown;
    server.use(
      http.post('/api/servers', async ({ request }) => {
        body = await request.json();
        return HttpResponse.json(serverView({ slug: 'yt' }), { status: 201 });
      }),
    );
    let saved = '';
    const { user } = renderWithApp(<ServerForm onSaved={(s) => (saved = s.slug)} />);
    await user.type(screen.getByLabelText('Slug'), 'yt');
    await user.type(screen.getByLabelText('Name'), 'YouTrack');
    await user.type(screen.getByLabelText('URL'), 'https://yt.example.com/mcp');
    await user.click(screen.getByRole('radio', { name: 'User OAuth' }));
    await user.type(screen.getByLabelText('Authorize URL'), 'https://hub.example.com/authorize');
    await user.type(screen.getByLabelText('Token URL'), 'https://hub.example.com/token');
    await user.type(screen.getByLabelText('Client ID'), 'cid');
    await user.type(screen.getByLabelText('Client secret'), 'csecret');
    await user.type(screen.getByLabelText('Scopes'), 'YouTrack Hub');
    await user.click(screen.getByRole('button', { name: 'Add parameter' }));
    await user.type(screen.getByLabelText('Parameter key 1'), 'access_type');
    await user.type(screen.getByLabelText('Parameter value 1'), 'offline');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(saved).toBe('yt'));
    expect(body).toMatchObject({
      slug: 'yt',
      secret: 'csecret',
      auth: {
        kind: 'oauth_user',
        header: 'Authorization',
        template: 'Bearer {secret}',
        oauth: {
          clientId: 'cid',
          scopes: ['YouTrack', 'Hub'],
          extraAuthorizeParams: { access_type: 'offline' },
        },
      },
    });
  });

  it('a failing /secret after a successful PUT invalidates the server and its tools', async () => {
    server.use(
      http.put('/api/servers/gitea', () => HttpResponse.json(serverView())),
      http.put('/api/servers/gitea/secret', () => errorResponse(500, 'boom')),
    );
    const { user, queryClient } = renderWithApp(
      <ServerForm
        server={serverView({
          auth: { kind: 'shared_secret', header: 'Authorization', template: 'Bearer {secret}' },
        })}
        onSaved={noop}
      />,
    );
    queryClient.setQueryData(['servers', 'gitea'], serverView());
    queryClient.setQueryData(['servers', 'gitea', 'tools'], []);
    await user.type(screen.getByLabelText('Secret value'), 'new');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await screen.findByRole('alert');
    expect(queryClient.getQueryState(['servers', 'gitea'])?.isInvalidated).toBe(true);
    expect(queryClient.getQueryState(['servers', 'gitea', 'tools'])?.isInvalidated).toBe(true);
  });

  it('a server changed by another admin meanwhile (409 server_conflict) asks to reload', async () => {
    server.use(
      http.put('/api/servers/gitea', () =>
        errorResponse(409, 'server_conflict', 'server "gitea" was changed meanwhile'),
      ),
    );
    const { user } = renderWithApp(<ServerForm server={serverView()} onSaved={noop} />);
    await user.click(screen.getByRole('button', { name: 'Save' }));
    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('This server was changed by someone else');
  });

  it('the server rejection of the openid scope is shown under the scopes field', async () => {
    server.use(
      http.post('/api/servers', () =>
        errorResponse(422, 'invalid_auth', 'the openid scope is not supported for upstream OAuth'),
      ),
    );
    const { user } = renderWithApp(<ServerForm onSaved={noop} />);
    await user.type(screen.getByLabelText('Slug'), 'yt');
    await user.type(screen.getByLabelText('Name'), 'YouTrack');
    await user.type(screen.getByLabelText('URL'), 'https://yt.example.com/mcp');
    await user.click(screen.getByRole('radio', { name: 'User OAuth' }));
    await user.type(screen.getByLabelText('Authorize URL'), 'https://hub.example.com/authorize');
    await user.type(screen.getByLabelText('Token URL'), 'https://hub.example.com/token');
    await user.type(screen.getByLabelText('Client ID'), 'cid');
    await user.type(screen.getByLabelText('Scopes'), 'openid');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    const scopes = await screen.findByLabelText('Scopes');
    await waitFor(() => expect(scopes.getAttribute('aria-invalid')).toBe('true'));
    expect(
      document.getElementById(scopes.getAttribute('aria-describedby')?.split(' ')[0] ?? '')
        ?.textContent,
    ).toContain('openid');
  });
});
