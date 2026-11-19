// Scenario 10: an admin disables a person; that person's open tab shows the sign-in page at its
// next action, and they cannot come back with a new link.
import { expect, strings, test, control, getStacks, newPerson, signInWith } from './fixtures.ts';

test('disabling a person ends the session of their open tab at its next action', async ({
  page,
  guard,
}) => {
  const stacks = await getStacks();
  guard.excuseRefusal(
    401,
    /^\/api\/catalog$/,
    "the request the disabled person's next click makes",
  );
  const base = stacks.plain.baseURL;
  const victim = await newPerson('plain', 'Soon Disabled', false);
  const admin = await newPerson('plain', 'Disabling Admin', true);
  await signInWith(page, victim.link);
  await page.getByRole('link', { name: strings.nav.tokens }).click();
  await expect(page.getByRole('heading', { level: 1, name: strings.tokens.title })).toBeVisible();

  // Control: before the disabling, the next action works.
  await page.getByRole('link', { name: strings.nav.catalog }).click();
  await expect(page.getByRole('heading', { level: 1, name: strings.catalog.title })).toBeVisible();
  await page.getByRole('link', { name: strings.nav.tokens }).click();
  await expect(page.getByRole('heading', { level: 1, name: strings.tokens.title })).toBeVisible();

  // The admin disables them in the UI.
  const adminPage = await (await guard.newContext()).newPage();
  await signInWith(adminPage, admin.link);
  await adminPage.goto(`${base}/admin/users`);
  const row = adminPage.getByRole('row', { name: /Soon Disabled/ });
  await row.getByRole('button', { name: strings.users.disable }).click();
  await adminPage
    .getByRole('alertdialog')
    .getByRole('button', { name: strings.users.disable })
    .click();
  await expect(adminPage.getByText(strings.users.disabledToast)).toBeVisible();
  await expect(row.getByText(strings.users.disabled, { exact: true })).toBeVisible();

  // The open tab: the next action leads to the sign-in page.
  await page.getByRole('link', { name: strings.nav.catalog }).click();
  await expect(page.getByRole('heading', { level: 1, name: strings.login.title })).toBeVisible();
  await expect(page.getByRole('complementary')).toHaveCount(0);

  // No way back with a new link, and the admin carries on.
  await expect(control.loginLink('plain', victim.sub)).rejects.toThrow(/login-link/);
  await adminPage.getByRole('link', { name: strings.nav.catalog }).click();
  await expect(
    adminPage.getByRole('heading', { level: 1, name: strings.catalog.title }),
  ).toBeVisible();
});
