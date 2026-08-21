/**
 * The zone list and the zone.
 */

import { expect, reset, signIn, test } from "./fixtures";

test.describe.configure({ mode: "serial" });

// This file walks a zone from nothing to gone, so it starts from nothing.
test.beforeAll(async ({ server }) => {
  await reset(server);
});

test("a zone can be created, and the interface lands on it", async ({ page, server }) => {
  await signIn(page, server);
  await page.getByRole("link", { name: "Zones" }).click();

  await page.getByRole("button", { name: "+ New zone" }).first().click();
  await page.getByLabel("Name", { exact: true }).fill("example.com");
  await page.getByLabel("Default TTL").fill("300");
  // Nothing is invented for this, so a zone created without it is lame and
  // says so, giving it here is what makes the zone answerable.
  await page.getByLabel("Address of the name server").fill("192.0.2.10");
  await page.getByRole("button", { name: "Create zone" }).click();

  // The apex is absolute whether or not the trailing dot was typed.
  await expect(page).toHaveURL(/\/zones\/example\.com\.$/);
  await expect(page.getByText("example.com.").first()).toBeVisible();
  // Creating the zone is one commit and the address record is a second, so
  // the zone lands on serial 2 (D2).
  await expect(page.getByRole("definition").filter({ hasText: /^2$/ })).toBeVisible();
  await expect(page.getByRole("cell", { name: "192.0.2.10" })).toBeVisible();
  // And nothing is complaining about a name server with no address.
  await expect(page.getByText("A name server here has no address")).toHaveCount(0);
});

test("the list shows it, and the filter narrows without a request", async ({ page, server }) => {
  await signIn(page, server);
  await page.goto(`${server.url}/zones`);

  await expect(page.getByRole("button", { name: "example.com.", exact: true })).toBeVisible();
  await expect(page.getByText("Forward", { exact: true })).toBeVisible();

  const filter = page.getByLabel("Search zones");
  await filter.fill("nothing-like-this");
  await expect(page.getByRole("heading", { name: "Nothing matches" })).toBeVisible();

  await filter.fill("example");
  await expect(page.getByRole("button", { name: "example.com.", exact: true })).toBeVisible();
});

test("the zone's settings save and come back", async ({ page, server }) => {
  await signIn(page, server);
  await page.goto(`${server.url}/zones/example.com./settings`);

  await page.getByLabel("Mailbox").fill("dns.example.com.");
  await page.getByRole("button", { name: "Save changes" }).click();
  await expect(page.getByText("Saved")).toBeVisible();

  await page.reload();
  await expect(page.getByLabel("Mailbox")).toHaveValue("dns.example.com.");
  // One commit, one step (D2).
  await expect(page.getByRole("definition").filter({ hasText: /^3$/ })).toBeVisible();
});

test("automatic reverse has three states, and the middle one is not off", async ({
  page,
  server,
}) => {
  await signIn(page, server);
  await page.goto(`${server.url}/zones/example.com./settings`);

  const off = page.getByRole("button", { name: "Off", exact: true });
  await off.click();
  await page.getByRole("button", { name: "Save changes" }).click();
  await expect(page.getByText("Saved")).toBeVisible();
  await page.reload();
  await expect(off).toHaveAttribute("aria-pressed", "true");

  // Back to following the server, which is a third state and has to survive a
  // round trip as one: sending false instead would silently change meaning.
  const follow = page.getByRole("button", { name: "Server default" });
  await follow.click();
  await page.getByRole("button", { name: "Save changes" }).click();
  await expect(page.getByText("Saved")).toBeVisible();
  await page.reload();
  await expect(follow).toHaveAttribute("aria-pressed", "true");
  await expect(off).toHaveAttribute("aria-pressed", "false");
});

test("a zone is not deleted until its name is typed", async ({ page, server }) => {
  await signIn(page, server);
  await page.goto(`${server.url}/zones`);

  await page.getByRole("button", { name: "Delete example.com." }).click();

  const confirm = page.getByRole("button", { name: "Delete zone" });
  await expect(confirm).toBeDisabled();

  await page.getByLabel("Type the name to confirm", { exact: true }).fill("example.com");
  await expect(confirm).toBeDisabled(); // the apex is absolute; this is not it

  await page.getByLabel("Type the name to confirm", { exact: true }).fill("example.com.");
  await expect(confirm).toBeEnabled();
  await confirm.click();

  await expect(page.getByRole("heading", { name: "No zones yet" })).toBeVisible();
});

test("a zone that is not here says so, without losing the shell", async ({ page, server }) => {
  await signIn(page, server);
  await page.goto(`${server.url}/zones/nowhere.example.`);

  await expect(page.getByRole("heading", { name: "Not here" })).toBeVisible();
  await expect(page.getByText("not authoritative for nowhere.example.")).toBeVisible();
  // Still inside the interface: losing your way around is not part of the
  // answer to "that zone is gone".
  await expect(page.getByRole("link", { name: "Zones" })).toBeVisible();
});

// Working out that 192.168.0.0/16 is 168.192.in-addr.arpa. by hand is the
// first thing that sends somebody to a manual.
test("a network becomes the reverse zone that answers for it", async ({ page, server }) => {
  await signIn(page, server);
  await page.goto(`${server.url}/zones`);

  await page.getByRole("button", { name: "+ New zone" }).first().click();
  await page.getByLabel("Name", { exact: true }).fill("192.168.0.0/16");
  await page.getByRole("button", { name: "Create zone" }).click();

  await expect(page).toHaveURL(/\/zones\/168\.192\.in-addr\.arpa\.$/);
  await expect(page.getByText("168.192.in-addr.arpa.").first()).toBeVisible();
  await expect(page.getByText("192.168.0.0/16")).toBeVisible();
});
