// The documented limitation of the OAuth callback (CLAUDE.md, "Callback limitation"): the session
// cookie is SameSite=Strict, so when the provider is not the same site as the gateway the browser
// does not send it on the way back, and the callback answers `?error=unauthenticated`.
// The ordinary setup here: gateway on localhost, provider on host.docker.internal.
import { expect, strings, test, control, getStacks, newPerson, signInWith } from './fixtures.ts';

const c = strings.connections;

test('a provider on another site brings the person back unauthenticated, with the documented message', async ({
  page,
  guard,
}) => {
  // The answer to Connect is the provider's address and the page leaves for it at once: its body
  // is lost to that navigation by design (it is listed, and carries no secret).
  guard.allowLostBody(
    /^\/api\/connections\/[^/]+\/start$/,
    'the page navigates to the address the answer carries',
    page,
  );
  const stacks = await getStacks();
  const base = stacks.plain.baseURL;
  expect(base).toMatch(/^http:\/\/localhost:\d+$/);
  expect(stacks.plain.providerURL).toMatch(/^http:\/\/host\.docker\.internal:\d+\/authorize$/);
  const server = await control.registerServer('plain', 'oauth_user');
  const person = await newPerson('plain', 'Cross Site', false);
  await signInWith(page, person.link);

  await page.getByRole('link', { name: strings.nav.connections }).click();
  const row = page.getByRole('row', { name: new RegExp(server.name) });
  await row.getByRole('button', { name: c.connectAria(server.name) }).click();
  await page.waitForURL((u) => u.href.startsWith(`${stacks.plain.providerURL}?`));
  await page.getByRole('link', { name: 'Approve' }).click();

  await expect(page.getByText(c.errors.unauthenticated)).toBeVisible();
  await expect(page.getByText(c.errorTitle)).toBeVisible();
  await expect(page).toHaveURL(`${base}/connections`); // the query string is cleared here too
  // Nothing was connected, and the person is still signed in on this page.
  await expect(row.getByText(c.status.missing)).toBeVisible();
  await expect(
    page.getByRole('complementary').getByText('Cross Site', { exact: true }),
  ).toBeVisible();
});
