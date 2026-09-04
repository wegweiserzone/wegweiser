/**
 * Where each secondary stands, and that the asking is actually running.
 *
 * This is the one place the whole path is exercised: a real server, its
 * prober, the notify list reaching it, and the interface reading what it
 * found (docs/decisions/, D36).
 */

import { expect, reset, seed, signIn, test } from "./fixtures";

test.describe.configure({ mode: "serial" });

test.beforeAll(async ({ server }) => {
  await reset(server);
});

test("with nobody on the notify list there is nobody to ask", async ({ page, server }) => {
  await signIn(page, server);
  await page.getByRole("link", { name: "Secondaries" }).click();

  await expect(page).toHaveURL(/\/secondaries$/);
  await expect(page.getByRole("heading", { name: "Nobody to ask" })).toBeVisible();
  await expect(page.getByText(/that list starts empty/)).toBeVisible();
});

// A pair the server has not heard back about yet is its own state. Reporting it
// as up to date would be the one thing this screen exists to stop.
test("a named secondary appears, and starts out unasked", async ({ page, server }) => {
  await seed(server, "POST", "/zones", { name: "watched.example" });
  await seed(server, "PATCH", "/settings", { notifyTargets: ["198.51.100.53"] });

  await signIn(page, server);
  await page.goto(`${server.url}/secondaries`);

  const row = page.getByRole("row").filter({ hasText: "watched.example." });
  await expect(row).toBeVisible();
  await expect(row.getByText("198.51.100.53:53")).toBeVisible();
  await expect(row.getByText("Unasked")).toBeVisible();
  await expect(row.getByText("never")).toBeVisible();

  // And the reader is told what that means rather than left to guess.
  await expect(page.getByText(/not known to be in step/)).toBeVisible();
});
