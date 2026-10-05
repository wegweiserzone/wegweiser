/**
 * The cluster page, on the server the browser tests run, which is a single
 * one: its configuration has no cluster section. That is a way to run rather
 * than a fault, and the page says what it would take to change it.
 */

import { expect, signIn, test } from "./fixtures";

test("a single server says what a cluster would take", async ({ page, server }) => {
  await signIn(page, server);
  await page.getByRole("link", { name: "Cluster" }).click();

  await expect(page).toHaveURL(/\/cluster$/);
  await expect(page.getByRole("heading", { name: "A single server" })).toBeVisible();
  await expect(page.getByText(/has no cluster section/)).toBeVisible();
  await expect(page.getByText("openssl rand -base64 32", { exact: false })).toBeVisible();
  // Nothing to start on a server that has no address for the others to reach.
  await expect(page.getByRole("button", { name: "Start a cluster" })).toHaveCount(0);
});
