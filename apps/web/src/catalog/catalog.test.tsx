import { screen } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { describe, expect, it } from 'vite-plus/test';
import { server } from '../../test/msw/handlers.ts';
import { renderWithApp } from '../../test/setup.ts';
import { CatalogPage } from './CatalogPage.tsx';

describe('CatalogPage', () => {
  it('lists servers with their tools, and filters by search', async () => {
    server.use(
      http.get('/api/catalog', () =>
        HttpResponse.json([
          {
            slug: 'gitea',
            name: 'Gitea',
            description: 'Git',
            tools: [
              {
                ref: 'gitea.create_issue',
                title: null,
                description: 'Create an issue',
                annotations: { destructiveHint: true },
              },
            ],
          },
          { slug: 'grafana', name: 'Grafana', description: '', tools: [] },
        ]),
      ),
    );
    const { user } = renderWithApp(<CatalogPage />, { route: '/catalog' });
    expect(await screen.findByText('gitea.create_issue')).toBeTruthy();
    expect(screen.getByText('destructive')).toBeTruthy();
    expect(screen.getByText('Grafana')).toBeTruthy();
    await user.type(screen.getByRole('searchbox', { name: 'Search the catalog' }), 'create');
    expect(screen.queryByText('Grafana')).toBeNull();
    await user.clear(screen.getByRole('searchbox'));
    await user.type(screen.getByRole('searchbox'), 'zzz');
    expect(screen.getByText('Nothing found')).toBeTruthy();
  });
});
