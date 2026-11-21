// Scenario 7: two tabs edit one server. PARTIALLY COVERED. The brief's path, "the second tab gets
// a conflict message", cannot be reached today: the UI sends no version, so the image only guards
// races inside a single request and a stale tab overwrites silently (a lost update; Findings of the
// Task 13 report, "optimistic concurrency not reachable from the UI"). This flow therefore asserts
// only what a correct server plus this UI must do, whichever way the conflict is handled later:
//   - the first tab's save succeeds and survives a reload;
//   - the stale tab's save either succeeds or shows the `server_conflict` text;
//   - the stored server afterwards equals exactly one of the two tabs' complete forms, never a
//     mixture of fields that neither tab submitted;
//   - if the conflict text was shown, the first tab's change is still there.
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

const f = strings.form;

test('two tabs edit one server: both saves end in a consistent state, or the second is told of a conflict', async ({
  page,
  guard,
}) => {
  const stacks = await getStacks();
  const base = stacks.plain.baseURL;
  const server = await control.registerServer('plain', 'none');
  const admin = await newPerson('plain', 'Concurrent Editor', true);
  await signInWith(page, admin.link);
  const second = await guard.newTab(page); // another tab of the same session

  const url = `${base}/admin/servers/${server.slug}`;
  await page.goto(url);
  await shellReady(page);
  await second.goto(url);
  await shellReady(second);
  const name = (p: typeof page) => p.getByLabel(f.name, { exact: true });
  const description = (p: typeof page) => p.getByLabel(f.description);
  await expect(name(second)).toHaveValue(server.name);
  const originalDescription = await description(second).inputValue();

  // Tab one, opened first, saves a new name and sees it after a reload.
  await name(page).fill('Name from tab one');
  await page.getByRole('button', { name: strings.common.save }).click();
  await expect(page.getByText(strings.servers.saved)).toBeVisible();
  await page.reload();
  await shellReady(page);
  await expect(name(page)).toHaveValue('Name from tab one');

  // Tab two, opened before that save, saves a different field.
  await description(second).fill('Description from tab two');
  await second.getByRole('button', { name: strings.common.save }).click();
  const saved = second.getByText(strings.servers.saved);
  const conflict = second.getByText(strings.errors.codes['server_conflict']!);
  await saved.or(conflict).waitFor();
  const conflictShown = await conflict.isVisible();

  // The stored state is one tab's complete form.
  await page.reload();
  await shellReady(page);
  const stored = {
    name: await name(page).inputValue(),
    description: await description(page).inputValue(),
  };
  expect([
    { name: 'Name from tab one', description: originalDescription }, // tab one's form
    { name: server.name, description: 'Description from tab two' }, // tab two's form
  ]).toContainEqual(stored);
  if (conflictShown) expect(stored.name).toBe('Name from tab one');
});
