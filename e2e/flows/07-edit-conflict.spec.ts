// Scenario 7: two tabs edit one server. The brief expected the second save to be refused with a
// conflict message; the image does not send the version to the client, so optimistic
// concurrency only guards races inside one request, and the stale tab simply overwrites. This
// flow pins what really happens (see the report, Findings).
import { expect, strings, test, control, getStacks, newPerson, signInWith } from './fixtures.ts';

const f = strings.form;

test('a stale second tab saves over the first one without any conflict message', async ({
  page,
}) => {
  const stacks = await getStacks();
  const base = stacks.plain.baseURL;
  const server = await control.registerServer('plain', 'none');
  const admin = await newPerson('plain', 'Concurrent Editor', true);
  await signInWith(page, admin.link);
  const second = await page.context().newPage(); // another tab of the same session

  const url = `${base}/admin/servers/${server.slug}`;
  await page.goto(url);
  await second.goto(url);
  const name = (p: typeof page) => p.getByLabel(f.name, { exact: true });
  await expect(name(page)).toHaveValue(server.name);
  await expect(name(second)).toHaveValue(server.name);

  // Tab one saves a new name.
  await name(page).fill('Name from tab one');
  await page.getByRole('button', { name: strings.common.save }).click();
  await expect(page.getByText(strings.servers.saved)).toBeVisible();

  // Tab two, still holding the old data, saves a new description.
  await second.getByLabel(f.description).fill('Description from tab two');
  await second.getByRole('button', { name: strings.common.save }).click();
  await expect(second.getByText(strings.servers.saved)).toBeVisible();
  await expect(second.getByText(strings.errors.codes['server_conflict']!)).toHaveCount(0);

  // The result: tab two's whole form won, so tab one's name is gone.
  await page.reload();
  await expect(name(page)).toHaveValue(server.name);
  await expect(page.getByLabel(f.description)).toHaveValue('Description from tab two');
});
