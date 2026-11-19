// Scenario 11: a session that has expired (moved past the idle limit in the database) ends at the
// next action with the sign-in page, not with an error.
import { expect, strings, test, control, getStacks, newPerson, signInWith } from './fixtures.ts';

test('an expired session shows the sign-in page at the next action', async ({ page, guard }) => {
  const stacks = await getStacks();
  guard.excuseRefusal(
    401,
    /^\/api\/catalog$/,
    'the request the next click makes on an expired session',
    page,
  );
  const base = stacks.plain.baseURL;
  const person = await newPerson('plain', 'Idle Person', false);
  await signInWith(page, person.link);
  await page.getByRole('link', { name: strings.nav.tokens }).click();
  await expect(page.getByRole('heading', { level: 1, name: strings.tokens.title })).toBeVisible();

  // Control: another person's session is not touched by the expiry below.
  const other = await newPerson('plain', 'Active Person', false);
  const otherPage = await (await guard.newContext()).newPage();
  await signInWith(otherPage, other.link);

  await control.expireSession('plain', person.sub);

  await page.getByRole('link', { name: strings.nav.catalog }).click();
  await expect(page.getByRole('heading', { level: 1, name: strings.login.title })).toBeVisible();
  await expect(page.getByText(strings.errors.unknown)).toHaveCount(0);
  await expect(page.getByText(strings.login.errorTitle)).toHaveCount(0);
  await expect(page).toHaveURL(`${base}/catalog`); // the same address; signing in returns here

  await otherPage.getByRole('link', { name: strings.nav.tokens }).click();
  await expect(
    otherPage.getByRole('heading', { level: 1, name: strings.tokens.title }),
  ).toBeVisible();
});
