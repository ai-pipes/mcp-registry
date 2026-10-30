import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { describe, expect, it } from 'vite-plus/test';
import { server } from '../../test/msw/handlers.ts';
import { App } from './App.tsx';

describe('App', () => {
  it('runs the routes on the browser router: signed in, / opens the catalog in the shell', async () => {
    server.use(
      http.get('/api/auth/me', () =>
        HttpResponse.json({ userId: 'u1', name: 'Anna', role: 'member', groups: [] }),
      ),
      http.get('/api/catalog', () => HttpResponse.json([])),
    );
    render(
      <QueryClientProvider client={new QueryClient()}>
        <App />
      </QueryClientProvider>,
    );
    expect(await screen.findByRole('navigation', { name: 'Navigation' })).toBeTruthy();
    expect(await screen.findByRole('heading', { name: 'Catalog', level: 1 })).toBeTruthy();
  });
});
