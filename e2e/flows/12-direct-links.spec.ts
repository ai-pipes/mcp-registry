// Scenario 12: a page reload on a deep link serves the app; an unknown API path does not.
import { expect, strings, test, getStacks, newPerson, signInWith, shellReady } from './fixtures.ts';

test('a reload on /admin/servers/new and /connections serves the app, /api/unknown does not', async ({
  page,
}) => {
  const stacks = await getStacks();
  const base = stacks.plain.baseURL;
  const admin = await newPerson('plain', 'Deep Linker', true);
  await signInWith(page, admin.link);

  await page.goto(`${base}/admin/servers/new`);
  await shellReady(page);
  await expect(
    page.getByRole('heading', { level: 1, name: strings.servers.createTitle }),
  ).toBeVisible();
  await shellReady(page);
  await page.reload();
  await shellReady(page);
  await expect(
    page.getByRole('heading', { level: 1, name: strings.servers.createTitle }),
  ).toBeVisible();
  await shellReady(page);
  await expect(page).toHaveURL(`${base}/admin/servers/new`);

  await page.goto(`${base}/connections`);
  await shellReady(page);
  await shellReady(page);
  await expect(page.getByText(strings.connections.empty).or(page.getByRole('table'))).toBeVisible();
  await page.reload();
  await shellReady(page);
  await expect(
    page.getByRole('heading', { level: 1, name: strings.connections.title }),
  ).toBeVisible();
  await shellReady(page);
  await expect(page.getByText(strings.connections.empty).or(page.getByRole('table'))).toBeVisible();
  await expect(page).toHaveURL(`${base}/connections`);

  // An unknown page is the app's own "not found"; an unknown API path is a JSON 404, not the app.
  await page.goto(`${base}/no-such-page`);
  await shellReady(page);
  await expect(page.getByText(strings.access.notFound)).toBeVisible();
  const unknown = await page.request.get(`${base}/api/unknown`);
  expect(unknown.status()).toBe(404);
  expect(unknown.headers()['content-type']).toContain('application/json');
  expect(await unknown.text()).not.toContain('<div id="root">');
});

test('a signed-out reload on a deep link shows the sign-in page at the same address', async ({
  page,
}) => {
  const stacks = await getStacks();
  const base = stacks.plain.baseURL;
  await page.goto(`${base}/admin/servers/new`);
  await expect(page.getByRole('heading', { level: 1, name: strings.login.title })).toBeVisible();
  await page.reload();
  await expect(page.getByRole('heading', { level: 1, name: strings.login.title })).toBeVisible();
  await expect(page).toHaveURL(`${base}/admin/servers/new`);
});
