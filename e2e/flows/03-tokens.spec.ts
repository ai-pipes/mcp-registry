// Scenario 3: an API token is shown once, then only its prefix; revoking ends it.
import { expect, strings, test, getStacks, newPerson, signInWith } from './fixtures.ts';

const t = strings.tokens;

test('a token and its connect command are shown once; afterwards only the prefix exists anywhere', async ({
  page,
  request,
  guard,
}) => {
  const stacks = await getStacks();
  const base = stacks.plain.baseURL;
  const person = await newPerson('plain', 'Token Keeper', false);
  await signInWith(page, person.link);
  await page.getByRole('link', { name: strings.nav.tokens }).click();
  await expect(page.getByRole('heading', { level: 1, name: t.title })).toBeVisible();
  await expect(page.getByText(t.empty)).toBeVisible();

  await page.getByRole('button', { name: t.create }).click();
  const dialog = page.getByRole('dialog');
  await dialog.getByRole('textbox', { name: t.name }).fill('laptop');
  await dialog.getByRole('button', { name: t.submit }).click();

  // Shown once, with the command that registers the gateway for an agent.
  await expect(dialog.getByText(t.shownOnce)).toBeVisible();
  const token = await dialog.getByRole('textbox', { name: t.token }).inputValue();
  expect(token).toMatch(/^sk_[A-Za-z0-9_-]{20,}$/);
  const command = await dialog.getByRole('textbox', { name: t.connectCommand }).inputValue();
  expect(command).toBe(
    `claude mcp add --transport http mcp-registry ${base}/mcp --header "Authorization: Bearer ${token}"`,
  );
  expect(await page.content()).toContain(token); // not vacuous: it is on the page now

  // While the token is active it opens /mcp.
  const mcp = (bearer: string) =>
    request.post(`${base}/mcp`, {
      headers: {
        Authorization: `Bearer ${bearer}`,
        'Content-Type': 'application/json',
        Accept: 'application/json, text/event-stream',
      },
      data: { jsonrpc: '2.0', id: 1, method: 'tools/list' },
    });
  expect((await mcp(token)).status()).toBe(200);
  expect((await mcp(`${token}x`)).status()).toBe(401); // negative control: a wrong token is refused

  await dialog.getByRole('button', { name: t.done }).click();
  await expect(dialog).toBeHidden();
  await guard.secretGone(page, token, 'the token');
  await guard.secretGone(page, command, 'the connect command');

  // The list keeps the name and the prefix only.
  const row = page.getByRole('row', { name: /laptop/ });
  await expect(row).toBeVisible();
  const prefix = (await row.getByText(/^sk_\S+…$/).innerText()).replace('…', '');
  expect(prefix).toMatch(/^sk_/);
  expect(token.startsWith(prefix)).toBe(true);
  expect(prefix.length).toBeLessThan(token.length);
  await expect(row.getByText(t.active)).toBeVisible();

  // A reload (the list comes from the server again) still has no token anywhere.
  await page.reload();
  await expect(page.getByRole('row', { name: /laptop/ })).toBeVisible();
  await guard.secretGone(page, token, 'the token after a reload');

  // Revoking asks first; declining changes nothing, confirming ends the token.
  await row.getByRole('button', { name: t.revoke }).click();
  const confirm = page.getByRole('alertdialog');
  await expect(confirm.getByText(t.revokeDescription('laptop'))).toBeVisible();
  await confirm.getByRole('button', { name: strings.confirm.cancel }).click();
  await expect(confirm).toBeHidden();
  expect((await mcp(token)).status()).toBe(200);

  await row.getByRole('button', { name: t.revoke }).click();
  await page.getByRole('alertdialog').getByRole('button', { name: t.revoke }).click();
  await expect(page.getByText(t.revokedToast)).toBeVisible();
  await expect(row.getByText(t.revoked)).toBeVisible();
  await expect(row.getByRole('button', { name: t.revoke })).toHaveCount(0);
  expect((await mcp(token)).status()).toBe(401);
});
