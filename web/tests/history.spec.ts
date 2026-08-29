/**
 * The history, and putting a zone back.
 */

import { expect, reset, seed, signIn, test } from "./fixtures";

test.describe.configure({ mode: "serial" });

test.beforeAll(async ({ server }) => {
  await reset(server);
  const zone = (await seed(server, "POST", "/zones", { name: "example.com" })) as {
    id: string;
  };
  await seed(server, "POST", `/zones/${zone.id}/records`, {
    name: "www.example.com.",
    type: "A",
    data: "192.0.2.10",
  });
  await seed(server, "POST", `/zones/${zone.id}/records`, {
    name: "mail.example.com.",
    type: "A",
    data: "192.0.2.25",
  });
});

test("every write is there, newest first", async ({ page, server }) => {
  await signIn(page, server);
  await page.getByLabel("Sections").getByRole("link", { name: "History" }).click();

  await expect(page.getByRole("heading", { name: "History" })).toBeVisible();

  // Creating the zone and the two records: three commits, and each advances
  // the serial by exactly one (D2). Newest first. Named by zone, because this
  // listing carries every zone the server has ever had: a commit outlives the
  // zone it describes, so that "who deleted example.com" survives the delete.
  await expect(page.getByRole("button", { name: /^2→3 Edit example\.com\./ })).toBeVisible();
  await expect(page.getByRole("button", { name: /^1→2 Edit example\.com\./ })).toBeVisible();
  await expect(page.getByRole("button", { name: /^0→1 Created example\.com\./ })).toBeVisible();
});

test("choosing a commit shows what it changed", async ({ page, server }) => {
  await signIn(page, server);
  await page.goto(`${server.url}/history`);

  // The newest is chosen on arrival, which is almost always the one wanted.
  await expect(page.getByText(/mail\.example\.com\..*IN A\s+192\.0\.2\.25/)).toBeVisible();

  // A commit records the record changes and nothing else: the serial it
  // advanced is on the commit itself, not written out as an SOA event.
  await expect(page.getByText(/SOA/)).toHaveCount(0);
});

test("the zone's own tab arrives already narrowed", async ({ page, server }) => {
  await signIn(page, server);
  await page.goto(`${server.url}/zones/example.com.`);
  // The zone's own tab, not the rail's section: both are called History and
  // they go to different places on purpose.
  await page.getByLabel("Zone").getByRole("link", { name: "History" }).click();

  await expect(page).toHaveURL(/\/history\?zone=example\.com\.$/);
  // The filter arrived with the link, which needs the zone list to have been
  // resolved first: the URL carries a name and the API takes an identifier.
  await expect(page.getByLabel("Zone")).toHaveValue("example.com.");
  await expect(page.getByRole("button", { name: /^0→1 Created/ })).toBeVisible();
});

test("reverting writes forward rather than rewinding", async ({ page, server }) => {
  await signIn(page, server);
  await page.goto(`${server.url}/history`);

  // Serial 2 is the state after www was added and before mail existed. Named
  // by zone: this listing carries every zone the server has ever had, because
  // a commit outlives the zone it describes.
  await page.getByRole("button", { name: /^1→2 Edit example\.com\./ }).click();

  await page.getByRole("button", { name: "Revert to this state" }).click();
  await page.getByRole("button", { name: "Revert the zone" }).click();

  await expect(page.getByText("The zone is back at that state")).toBeVisible();
  // Forward, not back: the new serial is higher than the one restored, because
  // a secondary that has seen a higher serial would never accept a lower one
  // (RFC 1982).
  await expect(page.getByText("not rewound")).toBeVisible();

  // And the zone really moved: mail is gone.
  await page.goto(`${server.url}/zones/example.com.`);
  await expect(page.getByRole("cell", { name: "192.0.2.10" })).toBeVisible();
  await expect(page.getByRole("cell", { name: "192.0.2.25" })).toHaveCount(0);
});
