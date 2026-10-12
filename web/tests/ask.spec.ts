/**
 * Asking the server what it answers (D49), without dig.
 */

import { expect, reset, seed, signIn, test } from "./fixtures";

test.describe.configure({ mode: "serial" });

test.beforeAll(async ({ server }) => {
  await reset(server);
  const zone = (await seed(server, "POST", "/zones", { name: "example.com" })) as { id: string };
  for (const record of [
    { name: "www.example.com.", type: "A", data: "192.0.2.10" },
    { name: "web.example.com.", type: "CNAME", data: "www.example.com." },
  ]) {
    await seed(server, "POST", `/zones/${zone.id}/records`, record);
  }
});

test("a name is answered as the server answers it, with the kind of answer first", async ({
  page,
  server,
}) => {
  await signIn(page, server);
  await page.goto(`${server.url}/ask`);

  await page.getByLabel("Name to ask about").fill("www.example.com");
  await page.getByRole("button", { name: "Ask", exact: true }).click();

  await expect(page.getByText("Answered", { exact: true })).toBeVisible();
  await expect(page.getByText(/^NOERROR, authoritative, from/)).toBeVisible();
  await expect(page.getByRole("cell", { name: "192.0.2.10" })).toBeVisible();
  // The question is in the address, so it can be handed to somebody else.
  await expect(page).toHaveURL(/\/ask\?name=www\.example\.com&type=A$/);
});

test("an alias can be followed to what it points at", async ({ page, server }) => {
  await signIn(page, server);
  await page.goto(`${server.url}/ask?name=web.example.com&type=A`);

  await expect(page.getByText("An alias", { exact: true })).toBeVisible();
  // The CNAME and the address it led to, as the server sends them.
  await expect(page.getByRole("cell", { name: "192.0.2.10" })).toBeVisible();

  await page.getByRole("button", { name: "Ask about www.example.com.", exact: true }).click();
  await expect(page.getByText("Answered", { exact: true })).toBeVisible();
  await expect(page).toHaveURL(/name=www\.example\.com\.&type=A$/);
});

test("a name no zone here covers is refused, with the reason", async ({ page, server }) => {
  await signIn(page, server);
  await page.goto(`${server.url}/ask?name=example.org&type=MX`);

  await expect(page.getByText("Refused", { exact: true })).toBeVisible();
  await expect(page.getByText(/no zone on this server covers that name/)).toBeVisible();
});

test("a record asks about itself from its row", async ({ page, server }) => {
  await signIn(page, server);
  await page.goto(`${server.url}/zones/example.com.`);

  await page
    .getByRole("link", { name: "Ask what the server answers for www.example.com. A" })
    .click();
  await expect(page).toHaveURL(/\/ask\?name=www\.example\.com\.&type=A$/);
  await expect(page.getByText("Answered", { exact: true })).toBeVisible();
});

test("a name typed into the palette can be asked about", async ({ page, server }) => {
  await signIn(page, server);
  await expect(page.getByRole("heading", { name: "Overview" })).toBeVisible();

  await page.keyboard.press("Control+k");
  await page.getByRole("textbox", { name: "Command" }).fill("www.example.com");
  await page.keyboard.press("Enter");

  await expect(page).toHaveURL(/\/ask\?name=www\.example\.com/);
  await expect(page.getByText("Answered", { exact: true })).toBeVisible();
});
