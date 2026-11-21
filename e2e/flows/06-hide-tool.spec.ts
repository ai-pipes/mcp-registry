// Scenario 6: the "Hidden" switch hides a tool for good: it survives a reload and disappears
// from a member's catalog (and comes back when switched off).
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

test('a hidden tool stays hidden after a reload and leaves the member catalog', async ({
  page,
  guard,
}) => {
  const stacks = await getStacks();
  const base = stacks.plain.baseURL;
  const server = await control.registerServer('plain', 'none', { tools: ['alpha', 'beta'] });
  const admin = await newPerson('plain', 'Tool Hider', true);
  const member = await newPerson('plain', 'Tool Seeker', false);
  await signInWith(page, admin.link);
  const memberPage = await guard.newPage();
  await signInWith(memberPage, member.link);

  const memberSees = async (tool: string) => {
    await memberPage.goto(`${base}/catalog`);
    await shellReady(memberPage);
    await expect(memberPage.getByText(server.name)).toBeVisible();
    return memberPage.getByText(`${server.slug}.${tool}`, { exact: true });
  };
  // Before: the member sees both.
  await expect(await memberSees('alpha')).toBeVisible();
  await expect(await memberSees('beta')).toBeVisible();

  await page.goto(`${base}/admin/servers/${server.slug}`);
  await shellReady(page);
  const beta = page.getByRole('checkbox', { name: strings.tools.hiddenAria('beta') });
  const alpha = page.getByRole('checkbox', { name: strings.tools.hiddenAria('alpha') });
  await expect(beta).not.toBeChecked();
  await beta.click();
  await expect(page.getByText(strings.tools.hiddenSaved)).toBeVisible();
  await expect(beta).toBeChecked();
  await expect(alpha).not.toBeChecked();

  // After a reload the switch is still on, for that tool only.
  await page.reload();
  await shellReady(page);
  await expect(
    page.getByRole('checkbox', { name: strings.tools.hiddenAria('beta') }),
  ).toBeChecked();
  await expect(
    page.getByRole('checkbox', { name: strings.tools.hiddenAria('alpha') }),
  ).not.toBeChecked();

  // The member's catalog lost the hidden tool and kept the other one.
  await expect(await memberSees('alpha')).toBeVisible();
  await expect(await memberSees('beta')).toHaveCount(0);
  // The admin's own tool list still shows it (that is where it is switched back on).
  await expect(page.getByRole('row', { name: /beta/ })).toBeVisible();

  // Switched off, it returns.
  await page.getByRole('checkbox', { name: strings.tools.hiddenAria('beta') }).click();
  await expect(
    page.getByRole('checkbox', { name: strings.tools.hiddenAria('beta') }),
  ).not.toBeChecked();
  await expect(await memberSees('beta')).toBeVisible();
});
