/**
 * The cluster page, on the server the browser tests run, which is a single
 * one: its configuration has no cluster section. That is a way to run rather
 * than a fault, and the page says what it would take to change it.
 *
 * A cluster cannot be made of that one server without changing it for every
 * file that shares it, so the tests of a member stand in for the API's answers
 * about the cluster. What they check is what the page does with those answers,
 * and what it sends.
 */

import type { Page } from "@playwright/test";

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

/** asMember answers for the cluster as ns1 would, leading ns2 and ns3. */
async function asMember(page: Page, opts: { removed?: boolean } = {}) {
  const at = (applied: number) => ({ applied, committed: 12, removed: false });
  const members = [
    {
      id: "ns1",
      address: "192.0.2.1:8054",
      role: "voter",
      leader: !opts.removed,
      progress: at(12),
    },
    { id: "ns2", address: "192.0.2.2:8054", role: "voter", leader: false, progress: at(9) },
    {
      id: "ns3",
      address: "192.0.2.3:8054",
      role: "nonvoter",
      leader: false,
      trouble: "not reached in 2s",
    },
  ].filter((m) => !(opts.removed && m.id === "ns1"));
  const removed: string[] = [];

  await page.route("**/api/v1/cluster", (route) =>
    route.fulfill({
      json: {
        self: { id: "ns1", address: "192.0.2.1:8054" },
        replicating: true,
        removed: Boolean(opts.removed),
        applied: 12,
        committed: 12,
        members: members.filter((m) => !removed.includes(m.id)),
      },
    }),
  );
  await page.route("**/api/v1/cluster/members/*", (route) => {
    removed.push(new URL(route.request().url()).pathname.split("/").pop() ?? "");
    return route.fulfill({ status: 204 });
  });
  return removed;
}

// D47: how far each member has got, as it said when the page asked.
test("each member says how far it has got", async ({ page, server }) => {
  await asMember(page);
  await signIn(page, server);
  await page.getByRole("link", { name: "Cluster" }).click();

  await expect(page.getByRole("row", { name: /ns1/ })).toContainText("Current");
  await expect(page.getByRole("row", { name: /ns2/ })).toContainText("3 behind");
  await expect(page.getByRole("row", { name: /ns3/ })).toContainText("Not reached");
});

test("a member is taken out of the cluster from its row", async ({ page, server }) => {
  const removed = await asMember(page);
  await signIn(page, server);
  await page.getByRole("link", { name: "Cluster" }).click();

  await page.getByRole("row", { name: /ns2/ }).hover();
  await page.getByRole("button", { name: "Remove ns2" }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog.getByText("Take out a member that is off first")).toBeVisible();
  await dialog.getByRole("button", { name: "Remove it" }).click();

  await expect(dialog).toBeHidden();
  expect(removed).toEqual(["ns2"]);
  await expect(page.getByRole("row", { name: /ns2/ })).toHaveCount(0);

  // On its own row the same act is leaving.
  await page.getByRole("row", { name: /ns1/ }).hover();
  await page.getByRole("button", { name: "Leave the cluster" }).click();
  await expect(page.getByRole("dialog").getByRole("button", { name: "Leave" })).toBeVisible();
});

test("a server taken out of its cluster says so", async ({ page, server }) => {
  await asMember(page, { removed: true });
  await signIn(page, server);
  await page.getByRole("link", { name: "Cluster" }).click();

  await expect(page.getByText("This server has left its cluster")).toBeVisible();
  await expect(page.getByText(/discard its Raft\s+directory only/)).toBeVisible();
  await expect(page.getByText("as this server last knew them", { exact: false })).toBeVisible();
  await expect(page.getByRole("button", { name: /^Remove / })).toHaveCount(0);
});
