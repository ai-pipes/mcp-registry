import { defineConfig, devices } from '@playwright/test';

// Browser flows of the web UI against a real application image. They are started by the Go
// orchestrator (compat/e2e, `make e2e-flows`), which boots the stacks and the fakes and passes
// what these specs need through the environment; they cannot run on their own.
//   E2E_CONTROL_URL, E2E_CONTROL_TOKEN  the control API (see control.ts)
//   E2E_HOST_RESOLVER_RULES             Chromium --host-resolver-rules: the fakes and the
//                                       *.compat.test names of the same-site stack
//   E2E_OUTPUT_DIR                      where traces and screenshots of failed flows go
const rules = process.env['E2E_HOST_RESOLVER_RULES'];

export default defineConfig({
  testDir: '.',
  testMatch: '*.spec.ts',
  outputDir: process.env['E2E_OUTPUT_DIR'] ?? '../../node_modules/.cache/playwright/flows-results',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  forbidOnly: !!process.env['CI'],
  reporter: 'list',
  timeout: 60_000,
  expect: { timeout: 10_000 },
  use: {
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    launchOptions: { args: rules === undefined ? [] : [`--host-resolver-rules=${rules}`] },
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
});
