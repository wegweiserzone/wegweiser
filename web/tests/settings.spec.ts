/**
 * The server-wide settings, and that changing one reaches the write path.
 */

import { expect, reset, seed, signIn, test } from "./fixtures";

test.describe.configure({ mode: "serial" });

test.beforeAll(async ({ server }) => {
  await reset(server);
});

test("the rail leads there, and it opens on the default", async ({ page, server }) => {
  await signIn(page, server);
  await page.getByRole("link", { name: "Settings" }).click();

  await expect(page).toHaveURL(/\/settings$/);
  await expect(
    page.getByRole("heading", { name: "When an address already answers" }),
  ).toBeVisible();
  await expect(page.getByRole("button", { name: /Keep the first/ })).toHaveAttribute(
    "aria-pressed",
    "true",
  );
});

// The screen is worth nothing unless what it stores is what a write runs under.
// Under "Take it over" the second name takes the address, which under the
// default it would not (docs/decisions/ D3).
test("choosing a policy changes what a later write does", async ({ page, server }) => {
  await signIn(page, server);
  await page.goto(`${server.url}/settings`);

  await page.getByRole("button", { name: /Take it over/ }).click();
  await expect(page.getByRole("button", { name: /Take it over/ })).toHaveAttribute(
    "aria-pressed",
    "true",
  );

  // Stored, not merely shown: a reload asks the server again.
  await page.reload();
  await expect(page.getByRole("button", { name: /Take it over/ })).toHaveAttribute(
    "aria-pressed",
    "true",
  );

  const zone = (await seed(server, "POST", "/zones", { name: "example.com" })) as {
    id: string;
  };
  await seed(server, "POST", "/zones", { name: "192.0.2.0/24" });

  await seed(server, "POST", `/zones/${zone.id}/records`, {
    name: "www.example.com.",
    type: "A",
    data: "192.0.2.10",
  });
  const second = (await seed(server, "POST", `/zones/${zone.id}/records`, {
    name: "mail.example.com.",
    type: "A",
    data: "192.0.2.10",
  })) as {
    conflicts?: { policy: string }[];
    generated?: { data: string }[];
  };

  expect(second.conflicts?.[0]?.policy).toBe("last-wins");
  expect(second.generated?.[0]?.data).toBe("mail.example.com.");
});

// A zone transfer hands over the whole zone, so the list of who may is the one
// setting where the default has to be visible as well as correct (D26).
test("the transfer list starts at nobody and can be filled in", async ({ page, server }) => {
  await signIn(page, server);
  await page.goto(`${server.url}/settings`);

  const section = page.locator("section").filter({
    has: page.getByRole("heading", { name: "Who may pull a whole zone" }),
  });
  await expect(section.getByText("nobody", { exact: true })).toBeVisible();

  await section.getByLabel("Allowed clients").fill("192.0.2.0/24, 2001:db8::1");
  await section.getByRole("button", { name: "Save" }).click();

  // A bare address comes back as the host prefix it means, which is the
  // server's reading of it rather than the browser's.
  await expect(section.getByText("192.0.2.0/24")).toBeVisible();
  await expect(section.getByText("2001:db8::1/128")).toBeVisible();
  await expect(section.getByText("nobody", { exact: true })).toBeHidden();

  // It survives a reload, so it was stored rather than held on the screen.
  await page.reload();
  await expect(section.getByText("192.0.2.0/24")).toBeVisible();

  // And it can be taken away again, or a list could never be undone.
  await section.getByLabel("Allowed clients").fill("");
  await section.getByRole("button", { name: "Save" }).click();
  await expect(section.getByText("nobody", { exact: true })).toBeVisible();
});

// A secondary that is not told waits out its refresh timer, and the list of who
// is told is deliberately not the list of who may transfer (D27).
test("the notify list is its own, and starts at nobody", async ({ page, server }) => {
  await signIn(page, server);
  await page.goto(`${server.url}/settings`);

  const section = page.locator("section").filter({
    has: page.getByRole("heading", { name: "Who is told when a zone changes" }),
  });
  await expect(section.getByText("nobody", { exact: true })).toBeVisible();

  await section
    .getByLabel("Secondaries")
    .fill("192.0.2.53, [2001:db8::53]:5353 key:secondary.example.com.");
  await section.getByRole("button", { name: "Save" }).click();

  // The port the server assumes is left off again, and the one that was named
  // is kept. A key after the address is what signs the notification.
  await expect(section.getByText("192.0.2.53", { exact: true })).toBeVisible();
  await expect(
    section.getByText("[2001:db8::53]:5353 key:secondary.example.com.", { exact: true }),
  ).toBeVisible();

  // Filling this in does not fill in the other one: they are two lists.
  const transfers = page.locator("section").filter({
    has: page.getByRole("heading", { name: "Who may pull a whole zone" }),
  });
  await expect(transfers.getByText("nobody", { exact: true })).toBeVisible();

  await page.reload();
  await expect(section.getByText("192.0.2.53", { exact: true })).toBeVisible();
});

// A key grants a transfer from any address, which an address list cannot do.
// Creating one grants nothing on its own: it has to reach the transfer list
// (D28).
test("a transfer key is created, revealed and withdrawn", async ({ page, server }) => {
  await signIn(page, server);
  await page.goto(`${server.url}/keys`);

  await expect(page.getByText("No keys yet")).toBeVisible();
  await page.getByRole("button", { name: "Create one" }).click();
  await page.getByLabel("Name").fill("secondary.example.com.");
  await page.getByRole("button", { name: "Create key" }).click();

  // The secret is shown, and it is base64 of a 32 byte hmac-sha256 key.
  const banner = page.getByRole("alert").filter({ hasText: "The secret for" });
  const secret = await banner.locator("code").innerText();
  expect(secret).toMatch(/^[A-Za-z0-9+/]{42}[A-Za-z0-9+/=]{2}$/);

  await expect(
    page.getByRole("cell", { name: "secondary.example.com.", exact: true }),
  ).toBeVisible();
  await expect(page.getByText("hmac-sha256", { exact: true })).toBeVisible();

  // Unlike a token it can be read again: the server has to keep it to verify
  // a signature, so hiding it here would be theatre.
  await page.getByRole("button", { name: "Hide" }).click();
  await page.getByRole("button", { name: "Show secret" }).click();
  await expect(banner.locator("code")).toHaveText(secret);

  // Withdrawing clears it, so it cannot be read back afterwards.
  await page.getByRole("button", { name: "Withdraw secondary.example.com." }).click();
  await page.getByRole("button", { name: "Withdraw it" }).click();
  await expect(page.getByText("Withdrawn")).toBeVisible();
  await expect(page.getByRole("button", { name: "Show secret" })).toBeHidden();
});
