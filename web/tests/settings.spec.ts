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
// default it would not (docs/decisions.md D3).
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
