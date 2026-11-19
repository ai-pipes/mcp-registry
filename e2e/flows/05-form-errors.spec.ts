// Scenario 5: the server refuses a slug and a header name the form let through (the form only
// checks that they are present); each error is shown under its own field and nothing is created.
import { expect, strings, test, control, getStacks, newPerson, signInWith } from './fixtures.ts';

const f = strings.form;

// The texts of the domain rules (catalog/domain), which the UI shows as the server wrote them.
const SLUG_REFUSED =
  'server slug must be 2-32 characters: lowercase letters, digits and dashes, starting with a letter';
const HEADER_REFUSED = 'header name may contain letters, digits and dashes';

test('a bad slug and a bad header name are shown under their own fields and create nothing', async ({
  page,
  guard,
}) => {
  const stacks = await getStacks();
  guard.excuseRefusal(
    422,
    /^\/api\/servers$/,
    'the refusals of the bad slug and the bad header name',
    page,
  );
  const base = stacks.plain.baseURL;
  const upstream = await control.registerServer('plain', 'none');
  const admin = await newPerson('plain', 'Form Breaker', true);
  await signInWith(page, admin.link);
  await page.goto(`${base}/admin/servers/new`);

  const slug = page.getByLabel(f.slug);
  const header = page.getByLabel(f.header, { exact: true });
  const invalid = async (field: typeof slug) => field.getAttribute('aria-invalid');
  // The text of the error shown for a field: what its aria-describedby points to, first entry.
  const errorOf = async (field: typeof slug) => {
    const id = (await field.getAttribute('aria-describedby'))?.split(' ')[0];
    return id === undefined ? null : page.locator(`[id="${id}"]`).innerText();
  };

  await slug.fill('Bad Slug!');
  await page.getByLabel(f.name, { exact: true }).fill('Never Created');
  await page.getByLabel(f.url, { exact: true }).fill(upstream.url);
  await page.getByRole('radio', { name: strings.servers.authKind.shared_secret }).click();
  await page.getByLabel(f.sharedSecret).fill('a-secret-value');
  await page.getByRole('button', { name: strings.common.save }).click();

  // The slug is refused, and the error sits under the slug field and no other.
  await expect(slug).toHaveAttribute('aria-invalid', 'true');
  expect(await errorOf(slug)).toBe(SLUG_REFUSED);
  expect(await invalid(page.getByLabel(f.name, { exact: true }))).toBeNull();
  expect(await invalid(page.getByLabel(f.url, { exact: true }))).toBeNull();
  expect(await invalid(header)).toBeNull();
  await expect(page.getByText(strings.servers.created)).toHaveCount(0);

  // A good slug and a bad header name: now the error is under the header field.
  await slug.fill(`never-${Math.random().toString(36).slice(2, 8)}`);
  await header.fill('Bad Header!');
  await page.getByRole('button', { name: strings.common.save }).click();
  await expect(header).toHaveAttribute('aria-invalid', 'true');
  expect(await errorOf(header)).toBe(HEADER_REFUSED);
  await expect(slug).not.toHaveAttribute('aria-invalid', 'true');
  expect(await invalid(page.getByLabel(f.template, { exact: true }))).toBeNull();
  await expect(page).toHaveURL(`${base}/admin/servers/new`);

  // Negative control: with both fixed the same form creates the server.
  await header.fill('X-Api-Key');
  await page.getByRole('button', { name: strings.common.save }).click();
  await expect(page.getByText(strings.servers.created)).toBeVisible();
  // The page of the new server shows its name once the server's answer is in; wait for that before
  // navigating on, so the answer is consumed (a navigation drops an unfinished body).
  await expect(page.getByRole('heading', { level: 1, name: /Never Created/ })).toBeVisible();
  await page.goto(`${base}/admin/servers`);
  await expect(page.getByRole('row', { name: /Never Created/ })).toHaveCount(1);
});
