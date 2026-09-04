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

// The other half of the arrangement, and the one somebody otherwise copies out
// of the documentation and fills in by hand. It carries a secret, so it is
// behind the admin scope (D34).
test("the configuration for the second nameserver is written here", async ({
  page,
  server,
}) => {
  await seed(server, "POST", "/zones", { name: "secondary.example" });
  await seed(server, "POST", "/zones", { name: "198.51.100.0/24" });
  await seed(server, "POST", "/tsig-keys", { name: "ns2.secondary.example." });
  await seed(server, "PATCH", "/settings", {
    transferAllow: ["key:ns2.secondary.example."],
    notifyTargets: ["198.51.100.53"],
  });

  await signIn(page, server);
  await page.goto(`${server.url}/secondaries`);
  await page.getByRole("button", { name: "Set one up" }).click();

  const section = page.getByRole("dialog");
  await section.getByLabel("This server's address").fill("192.0.2.1");
  await section.getByLabel("The secondary's address").fill("198.51.100.53");
  await section.getByRole("button", { name: "Write it" }).click();

  const file = section.locator("pre");
  await expect(file).toContainText('zone "secondary.example." {');
  // The reverse zone as much as the forward one: it is the one somebody
  // setting a secondary up by hand leaves out.
  await expect(file).toContainText('zone "100.51.198.in-addr.arpa." {');
  await expect(file).toContainText("primaries { 192.0.2.1; };");
  // Stored with the trailing dot RFC 8945 gives it, and written without one.
  await expect(file).toContainText("algorithm hmac-sha256;");

  // A complete arrangement has nothing to report.
  await expect(section.getByText("the arrangement is not finished")).toBeHidden();

  await section.getByRole("button", { name: "Knot" }).click();
  await section.getByRole("button", { name: "Write it" }).click();
  await expect(file).toContainText("domain: secondary.example.");
  // Knot drops a notification without this, quietly, and the zone stays
  // correct while the news takes a refresh interval.
  await expect(file).toContainText("action: notify");
});

// The warnings are the point of naming the secondary: the file is perfect and
// the transfer is refused, and nothing else says so.
test("it says when the arrangement will not work", async ({ page, server }) => {
  await seed(server, "PATCH", "/settings", { transferAllow: [], notifyTargets: [] });

  await signIn(page, server);
  await page.goto(`${server.url}/secondaries`);
  await page.getByRole("button", { name: "Set one up" }).click();

  const section = page.getByRole("dialog");
  await section.getByLabel("This server's address").fill("192.0.2.1");
  await section.getByRole("button", { name: "Write it" }).click();

  await expect(section.getByText("the arrangement is not finished")).toBeVisible();
  await expect(section.getByText("nobody may transfer a zone")).toBeVisible();
  await expect(section.getByText("nobody is told when a zone changes")).toBeVisible();
  // Written anyway. Half an arrangement is what somebody setting one up has.
  await expect(section.locator("pre")).toContainText('zone "secondary.example." {');
});
