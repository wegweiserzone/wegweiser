import { defineConfig, devices } from "@playwright/test";

/**
 * The browser tests.
 *
 * A smoke suite, deliberately: what nothing else in this repository can catch
 * is "the interface does not render at all", and that is worth a browser.
 * Everything past it (which records a table shows, what a filter matches) is
 * cheaper and steadier to test against the API, which the Go tests already do.
 *
 * One engine. Firefox is the strictest of the three about content security
 * policy, which is the part of this interface most likely to break silently,
 * and it is the smallest download of the three.
 */
export default defineConfig({
  testDir: "./tests",
  // One server, and the tests sign in to it. Parallelism here would buy a few
  // seconds and cost the ability to reason about what happened.
  workers: 1,
  fullyParallel: false,
  // A smoke test that passes on the second attempt has not passed. If one of
  // these is flaky it is the test that is wrong, and hiding it behind a retry
  // is how a suite stops meaning anything.
  retries: 0,
  forbidOnly: !!process.env.CI,
  reporter: process.env.CI ? "line" : "list",
  timeout: 20_000,
  globalSetup: "./tests/global-setup.ts",
  globalTeardown: "./tests/global-teardown.ts",
  use: {
    ...devices["Desktop Firefox"],
    colorScheme: "dark",
    trace: "retain-on-failure",
  },
  projects: [{ name: "firefox" }],
});
