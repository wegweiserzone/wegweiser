/**
 * What is wrong with a zone, and the one thing the interface can fix from here.
 */

import { expect, reset, seed, signIn, test } from "./fixtures";

test.describe.configure({ mode: "serial" });

test.beforeAll(async ({ server }) => {
  await reset(server);
});

test("a zone that is sound says so, and says how much it looked at", async ({ page, server }) => {
  const zone = await seed(server, "POST", "/zones", { name: "sound.example" });
  // Without an address for its own name server the zone is lame, and the
  // check would rightly say so.
  await seed(server, "POST", `/zones/${zone.id}/records`, {
    name: "ns1.sound.example.",
    type: "A",
    data: "192.0.2.53",
  });

  await signIn(page, server);
  await page.goto(`${server.url}/zones/sound.example./check`);

  await expect(page.getByText("Nothing has been checked yet")).toBeVisible();
  await page.getByRole("button", { name: "Check this zone" }).click();

  await expect(page.getByText("Nothing to report")).toBeVisible();
  await expect(page.getByText(/records read/)).toBeVisible();
});

test("a name server with no address is a warning, not an error", async ({ page, server }) => {
  await seed(server, "POST", "/zones", { name: "lame.example" });

  await signIn(page, server);
  await page.goto(`${server.url}/zones/lame.example./check`);
  await page.getByRole("button", { name: "Check this zone" }).click();

  await expect(page.getByText("1 warning in")).toBeVisible();
  await expect(page.getByText(/ns1\.lame\.example\..*no address in this zone/)).toBeVisible();
  await expect(page.getByText("warning · nameserver")).toBeVisible();
});

test("a reverse zone the server filled has nothing missing", async ({ page, server }) => {
  const fwd = await seed(server, "POST", "/zones", { name: "hosts.example" });
  await seed(server, "POST", `/zones/${fwd.id}/records`, {
    name: "ns1.hosts.example.",
    type: "A",
    data: "192.0.2.53",
  });
  await seed(server, "POST", `/zones/${fwd.id}/records`, {
    name: "www.hosts.example.",
    type: "A",
    data: "10.0.0.1",
  });
  // The reverse zone arrives after the address and fills itself: there was no
  // change for the automation to react to, so creating it is the moment.
  await seed(server, "POST", "/zones", { name: "10.0.0.0/24" });

  await signIn(page, server);
  await page.goto(`${server.url}/zones/0.0.10.in-addr.arpa./check`);

  await page.getByLabel("Include the reverse entries this zone is missing").check();
  await page.getByRole("button", { name: "Check this zone" }).click();

  // The PTR is there, so nothing is missing and nothing offers to write it.
  await expect(page.getByText("1 warning in")).toBeVisible();
  await expect(page.getByText("warning · reverse")).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Fill them in" })).toHaveCount(0);
});

test("the name an address reverses to can be changed from the finding", async ({
  page,
  server,
}) => {
  const fwd = await seed(server, "POST", "/zones", { name: "claim.example" });
  await seed(server, "POST", `/zones/${fwd.id}/records`, {
    name: "ns1.claim.example.",
    type: "A",
    data: "198.51.100.53",
  });
  await seed(server, "POST", "/zones", { name: "203.0.113.0/24" });
  await seed(server, "POST", `/zones/${fwd.id}/records`, {
    name: "www.claim.example.",
    type: "A",
    data: "203.0.113.10",
  });
  // Two names, one address. First wins, so this one gets no entry.
  await seed(server, "POST", `/zones/${fwd.id}/records`, {
    name: "mail.claim.example.",
    type: "A",
    data: "203.0.113.10",
  });

  await signIn(page, server);
  await page.goto(`${server.url}/zones/113.0.203.in-addr.arpa./check`);
  await page.getByLabel("Include the reverse entries this zone is missing").check();
  await page.getByRole("button", { name: "Check this zone" }).click();

  await expect(page.getByText(/answered in reverse as www\.claim\.example\./)).toBeVisible();
  await page.getByRole("button", { name: "Make this the answer" }).click();

  await expect(page.getByText("The address now reverses to that name.")).toBeVisible();
  await expect(page.getByText(/answered in reverse as mail\.claim\.example\./)).toBeVisible();
});
