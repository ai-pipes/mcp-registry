// Scenario 8: connecting a service through OAuth in the browser, and disconnecting it. The session
// cookie is SameSite=Strict, so the provider must be the same site as the gateway: this flow runs
// on the stack whose public address is gateway.compat.test, with the provider at idp.compat.test
// (both mapped to this machine by Chromium's resolver rules). The documented cross-site
// limitation is pinned by 08b-connect-cross-site.spec.ts.
import {
  expect,
  strings,
  test,
  control,
  getStacks,
  newPerson,
  signInWith,
  shellReady,
} from './fixtures.ts';

const c = strings.connections;

test('Connect goes to the provider and back; the toast shows, the query string is gone, Disconnect undoes it', async ({
  page,
  guard,
}) => {
  // The answer to Connect is the provider's address and the page leaves for it at once: its body
  // is lost to that navigation by design (it is listed, and carries no secret).
  guard.allowLostBody(
    page,
    'POST',
    /^\/api\/connections\/[^/]+\/start$/,
    'the page navigates to the address the answer carries',
  );
  const stacks = await getStacks();
  const base = stacks.samesite.baseURL;
  expect(base).toMatch(/^http:\/\/gateway\.compat\.test:\d+$/);
  expect(stacks.samesite.providerURL).toMatch(/^http:\/\/idp\.compat\.test:\d+\/authorize$/);
  const server = await control.registerServer('samesite', 'oauth_user');
  const person = await newPerson('samesite', 'Connector', false);
  await signInWith(page, person.link);

  await page.getByRole('link', { name: strings.nav.connections }).click();
  const row = page.getByRole('row', { name: new RegExp(server.name) });
  await expect(row.getByText(c.status.missing)).toBeVisible();
  await expect(row.getByRole('button', { name: c.disconnectAria(server.name) })).toHaveCount(0);

  // The button leaves for the provider, whose page asks for consent; approving it sends the
  // person back to the gateway, on a navigation the provider's site started.
  await row.getByRole('button', { name: c.connectAria(server.name) }).click();
  await page.waitForURL((u) => u.href.startsWith(`${stacks.samesite.providerURL}?`));
  await page.getByRole('link', { name: 'Approve' }).click();
  await expect(page.getByText(c.connected(server.name))).toBeVisible();
  await expect(page).toHaveURL(`${base}/connections`); // the query string is cleared
  await expect(page.getByText(c.errorTitle)).toHaveCount(0);
  await expect(row.getByText(c.status.ready)).toBeVisible();
  await expect(row.getByRole('button', { name: c.reconnectAria(server.name) })).toBeVisible();

  // A reload shows the same state from the server.
  await page.reload();
  await shellReady(page);
  await expect(
    page.getByRole('row', { name: new RegExp(server.name) }).getByText(c.status.ready),
  ).toBeVisible();

  // Disconnect asks first, then returns to "Not connected".
  await row.getByRole('button', { name: c.disconnectAria(server.name) }).click();
  const confirm = page.getByRole('alertdialog');
  await expect(confirm.getByText(c.disconnectTitle(server.name))).toBeVisible();
  await confirm.getByRole('button', { name: strings.confirm.cancel }).click();
  await expect(row.getByText(c.status.ready)).toBeVisible();
  await row.getByRole('button', { name: c.disconnectAria(server.name) }).click();
  await page.getByRole('alertdialog').getByRole('button', { name: c.disconnect }).click();
  await expect(page.getByText(c.disconnected(server.name))).toBeVisible();
  await expect(row.getByText(c.status.missing)).toBeVisible();
  await expect(row.getByRole('button', { name: c.connectAria(server.name) })).toBeVisible();
});
