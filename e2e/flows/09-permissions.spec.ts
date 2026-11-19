// Scenario 9: a member has no administration; an admin sees the call log and the audit, with
// the records of what was just done.
import { expect, strings, test, control, getStacks, newPerson, signInWith } from './fixtures.ts';

const adminLinks = [strings.nav.servers, strings.nav.users, strings.nav.calls, strings.nav.audit];

test('a member sees no administration and is denied /admin/calls and /admin/audit', async ({
  page,
}) => {
  const stacks = await getStacks();
  const base = stacks.plain.baseURL;
  const member = await newPerson('plain', 'Plain Member', false);
  await signInWith(page, member.link);

  const shell = page.getByRole('complementary');
  await Promise.all(
    [strings.nav.catalog, strings.nav.connections, strings.nav.tokens].map((name) =>
      expect(shell.getByRole('link', { name })).toBeVisible(),
    ),
  );
  await expect(shell.getByText(strings.nav.admin, { exact: true })).toHaveCount(0);
  await Promise.all(
    adminLinks.map((name) => expect(shell.getByRole('link', { name })).toHaveCount(0)),
  );

  const denied = async (path: string) => {
    await page.goto(`${base}${path}`);
    await expect(page.getByText(strings.access.forbidden)).toBeVisible();
    await expect(page.getByText(strings.access.forbiddenHint)).toBeVisible();
  };
  await denied('/admin/calls');
  await denied('/admin/audit');
  await denied('/admin/servers');
  await denied('/admin/users');
  // Nothing of the log or the audit is on a denied page.
  await expect(page.getByRole('table')).toHaveCount(0);
});

test('an admin sees the menu, the call log with a call just made, and the audit of their own action', async ({
  page,
  guard,
  request,
}) => {
  const stacks = await getStacks();
  const base = stacks.plain.baseURL;
  const server = await control.registerServer('plain', 'none', { tools: ['alpha', 'beta'] });
  const admin = await newPerson('plain', 'Log Reader', true);
  await signInWith(page, admin.link);

  const shell = page.getByRole('complementary');
  await expect(shell.getByText(strings.nav.admin, { exact: true })).toBeVisible();
  await Promise.all(
    adminLinks.map((name) => expect(shell.getByRole('link', { name })).toBeVisible()),
  );

  // A call through /mcp with a token made in the UI.
  await page.goto(`${base}/tokens`);
  await page.getByRole('button', { name: strings.tokens.create }).click();
  const dialog = page.getByRole('dialog');
  await dialog.getByRole('textbox', { name: strings.tokens.name }).fill('caller');
  await dialog.getByRole('button', { name: strings.tokens.submit }).click();
  const token = await dialog.getByRole('textbox', { name: strings.tokens.token }).inputValue();
  await dialog.getByRole('button', { name: strings.tokens.done }).click();
  await guard.secretGone(page, token, 'the token of the call');
  const called = await request.post(`${base}/mcp`, {
    headers: {
      Authorization: `Bearer ${token}`,
      'Content-Type': 'application/json',
      Accept: 'application/json, text/event-stream',
    },
    data: {
      jsonrpc: '2.0',
      id: 1,
      method: 'tools/call',
      params: {
        name: 'call_tool',
        arguments: { tool: `${server.slug}.alpha`, arguments: { text: 'from the flow' } },
      },
    },
  });
  expect(called.status()).toBe(200);
  const seen = await control.upstreamCalls(server.slug);
  expect(seen.map((c) => c.tool)).toEqual(['alpha']);

  // An admin action to find in the audit: hiding a tool.
  await page.goto(`${base}/admin/servers/${server.slug}`);
  await page.getByRole('checkbox', { name: strings.tools.hiddenAria('beta') }).click();
  await expect(page.getByText(strings.tools.hiddenSaved)).toBeVisible();

  await shell.getByRole('link', { name: strings.nav.calls }).click();
  await expect(page.getByRole('heading', { level: 1, name: strings.calls.title })).toBeVisible();
  const call = page.getByRole('row', { name: new RegExp(server.slug) });
  await expect(call.getByRole('cell', { name: 'Log Reader' })).toBeVisible();
  await expect(call.getByRole('cell', { name: 'alpha', exact: true })).toBeVisible();
  await expect(call.getByText(strings.calls.outcome['ok']!)).toBeVisible();

  await shell.getByRole('link', { name: strings.nav.audit }).click();
  await expect(page.getByRole('heading', { level: 1, name: strings.audit.title })).toBeVisible();
  const hidden = page.getByRole('row', { name: new RegExp(`tool\\.hide.*${server.slug}\\.beta`) });
  await expect(hidden.getByRole('cell', { name: 'Log Reader' })).toBeVisible();
  // The same trail names who registered the server (the control API's admin, not this person).
  const created = page.getByRole('row', { name: new RegExp(`server\\.create.*${server.slug}`) });
  await expect(created).toBeVisible();
  await expect(created.getByRole('cell', { name: 'Log Reader' })).toHaveCount(0);
});
