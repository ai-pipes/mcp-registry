// Scenario 1: the first start without OIDC and signing in with the link from the log.
// The person it signs in is the admin with sub `local-admin`; the UI shows its name, which the
// server gives as "Admin" (the sub is not displayed anywhere).
import { expect, strings, test, control, getStacks } from './fixtures.ts';

test('the first-start link opens a confirmation, sends nothing until the click, and signs the first admin in', async ({
  page,
  guard,
}) => {
  const stacks = await getStacks();
  const base = stacks.plain.baseURL;
  const link = await control.firstStartLink('plain');
  expect(link.startsWith(`${base}/login#link=ll_`)).toBe(true);

  const sent: string[] = [];
  page.on('request', (r) => {
    if (new URL(r.url()).pathname === '/api/auth/link') sent.push(r.method());
  });

  // Opening the link only asks. The code is gone from the address bar before anything is sent.
  await page.goto(link);
  await expect(
    page.getByRole('heading', { level: 1, name: strings.login.linkTitle }),
  ).toBeVisible();
  await expect(page).toHaveURL(`${base}/login`);
  expect(await page.evaluate(() => window.location.hash)).toBe('');
  expect(page.url()).not.toContain('ll_');
  expect(sent).toEqual([]);

  // Negative control: declining sends nothing either and leaves the visitor signed out.
  await page.getByRole('button', { name: strings.login.linkCancel }).click();
  await expect(page.getByRole('heading', { level: 1, name: strings.login.title })).toBeVisible();
  expect(sent).toEqual([]);

  // The same link, opened again, still works: declining did not spend it.
  await page.goto(link);
  await expect(
    page.getByRole('heading', { level: 1, name: strings.login.linkTitle }),
  ).toBeVisible();
  expect(sent).toEqual([]);
  await page.getByRole('button', { name: strings.login.switchConfirm }).click();

  // Signed in: the shell names the person and the role.
  await expect(page.getByRole('heading', { level: 1, name: strings.catalog.title })).toBeVisible();
  await expect(page).toHaveURL(`${base}/catalog`);
  expect(sent).toEqual(['POST']);
  const shell = page.getByRole('complementary');
  await expect(shell.getByText('Admin', { exact: true })).toBeVisible();
  await expect(shell.getByText(strings.nav.roleAdmin, { exact: true })).toBeVisible();
  await expect(shell.getByRole('link', { name: strings.nav.servers })).toBeVisible();

  // Negative control: a link is good once. The same link in another browser is refused, with the
  // text of the sign-in page, and signs nobody in.
  guard.excuseRefusal(401, /^\/api\/auth\/link$/, 'the refused second use of a one-time link');
  const other = await guard.newContext();
  const page2 = await other.newPage();
  await page2.goto(link);
  await expect(
    page2.getByRole('heading', { level: 1, name: strings.login.linkTitle }),
  ).toBeVisible();
  await page2.getByRole('button', { name: strings.login.switchConfirm }).click();
  await expect(page2.getByText(strings.login.errors['invalid_link']!)).toBeVisible();
  await expect(page2.getByRole('heading', { level: 1, name: strings.login.title })).toBeVisible();
});
