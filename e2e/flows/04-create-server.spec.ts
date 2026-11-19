// Scenario 4: the server form creates a server of type "none"; its tools appear on the server
// page and in the catalog.
import { expect, strings, test, control, getStacks, newPerson, signInWith } from './fixtures.ts';

const f = strings.form;

test('a server made in the form shows its synced tools on its page and in the catalog', async ({
  page,
}) => {
  const stacks = await getStacks();
  const base = stacks.plain.baseURL;
  // A fake upstream with two tools. The server the control API registers on it is only the means
  // to get one; the flow below creates a second server, through the form, on the same upstream.
  const upstream = await control.registerServer('plain', 'none', { tools: ['alpha', 'beta'] });
  const slug = `form-${Math.random().toString(36).slice(2, 8)}`;
  const admin = await newPerson('plain', 'Server Author', true);
  await signInWith(page, admin.link);

  // Before: the catalog has the registered server and nothing of the new one.
  await page.getByRole('link', { name: strings.nav.catalog }).click();
  await expect(page.getByText(upstream.name)).toBeVisible();
  await expect(page.getByText(slug)).toHaveCount(0);

  await page.goto(`${base}/admin/servers`);
  await page.getByRole('link', { name: strings.servers.create }).click();
  await expect(
    page.getByRole('heading', { level: 1, name: strings.servers.createTitle }),
  ).toBeVisible();
  await page.getByLabel(f.slug).fill(slug);
  await page.getByLabel(f.name, { exact: true }).fill('Form Created');
  await page.getByLabel(f.description).fill('Made in the form');
  await page.getByLabel(f.url, { exact: true }).fill(upstream.url);
  await expect(page.getByRole('radio', { name: strings.servers.authKind.none })).toBeChecked();
  await page.getByRole('button', { name: strings.common.save }).click();

  // The server page: created, synced, with both tools.
  await expect(page.getByText(strings.servers.created)).toBeVisible();
  await expect(page).toHaveURL(`${base}/admin/servers/${slug}`);
  await expect(page.getByRole('heading', { level: 1, name: /Form Created/ })).toBeVisible();
  const tools = page.getByRole('table');
  await expect(tools.getByRole('row', { name: /alpha/ })).toBeVisible();
  await expect(tools.getByRole('row', { name: /beta/ })).toBeVisible();
  await expect(page.getByText(strings.servers.syncErrorTitle)).toHaveCount(0);

  // The list of servers: the new one with 2 tools.
  await page.goto(`${base}/admin/servers`);
  const row = page.getByRole('row', { name: /Form Created/ });
  await expect(row.getByText(strings.servers.authKind.none)).toBeVisible();
  await expect(row.getByRole('cell', { name: '2', exact: true })).toBeVisible();

  // The catalog: the server's card with its refs.
  await page.goto(`${base}/catalog`);
  // The search box narrows the catalog to this server, so what follows is about its card only.
  await page.getByRole('searchbox', { name: strings.catalog.search }).fill(slug);
  await expect(page.getByText(strings.catalog.tools(2))).toBeVisible();
  await expect(page.getByText(`${slug}.alpha`, { exact: true })).toBeVisible();
  await expect(page.getByText(`${slug}.beta`, { exact: true })).toBeVisible();
  await expect(page.getByText(upstream.name)).toHaveCount(0); // the other server is filtered out
});
