// Scenario 2: sign-in through the identity provider, with the return to the page asked for, and
// the refusal of a person outside the allowed groups.
import { expect, strings, test, control, getStacks } from './fixtures.ts';

test('a signed-out visit to /admin/servers signs in through the IdP and returns there', async ({
  page,
}) => {
  const stacks = await getStacks();
  const base = stacks.oidc.baseURL;
  const sub = `oidc-${Math.random().toString(36).slice(2, 8)}`;
  await control.nextLogin({
    sub,
    name: 'Oidc Admin',
    email: 'oidc.admin@example.test',
    groups: ['mcp-admins'],
  });

  await page.goto(`${base}/admin/servers`);
  await expect(page.getByRole('heading', { level: 1, name: strings.login.title })).toBeVisible();
  await expect(page).toHaveURL(`${base}/admin/servers`);
  // SSO is configured here: the button, not the link instructions.
  await expect(page.getByText(strings.login.localHint)).toHaveCount(0);
  await page.getByRole('link', { name: strings.login.submit }).click();

  // Back from the provider on the page that was asked for, signed in as the person it named.
  await expect(page.getByRole('heading', { level: 1, name: strings.servers.title })).toBeVisible();
  await expect(page).toHaveURL(`${base}/admin/servers`);
  const shell = page.getByRole('complementary');
  await expect(shell.getByText('Oidc Admin', { exact: true })).toBeVisible();
  await expect(shell.getByText(strings.nav.roleAdmin, { exact: true })).toBeVisible();
});

test('a person outside the allowed groups is refused with the text on the sign-in page', async ({
  page,
}) => {
  const stacks = await getStacks();
  const base = stacks.oidc.baseURL;
  await control.nextLogin({
    sub: `stranger-${Math.random().toString(36).slice(2, 8)}`,
    name: 'Stranger',
    email: 'stranger@example.test',
    groups: ['strangers'],
  });
  await page.goto(`${base}/catalog`);
  await page.getByRole('link', { name: strings.login.submit }).click();

  await expect(page.getByText(strings.login.errors['not_allowed']!)).toBeVisible();
  await expect(page.getByText(strings.login.errorTitle)).toBeVisible();
  await expect(page.getByRole('heading', { level: 1, name: strings.login.title })).toBeVisible();
  await expect(page).toHaveURL(`${base}/?loginError=not_allowed`);
  await expect(page.getByRole('complementary')).toHaveCount(0); // no shell: nobody signed in
});

test('a member signs in through the IdP and the administration stays closed to them', async ({
  page,
}) => {
  const stacks = await getStacks();
  const base = stacks.oidc.baseURL;
  await control.nextLogin({
    sub: `member-${Math.random().toString(36).slice(2, 8)}`,
    name: 'Oidc Member',
    email: 'oidc.member@example.test',
    groups: ['mcp-users'],
  });
  await page.goto(`${base}/admin/servers`);
  await page.getByRole('link', { name: strings.login.submit }).click();
  // Back on the page asked for, which is denied to a member.
  await expect(page.getByText(strings.access.forbidden)).toBeVisible();
  const shell = page.getByRole('complementary');
  await expect(shell.getByText('Oidc Member', { exact: true })).toBeVisible();
  await expect(shell.getByText(strings.nav.roleMember, { exact: true })).toBeVisible();
  await expect(shell.getByRole('link', { name: strings.nav.servers })).toHaveCount(0);
});
