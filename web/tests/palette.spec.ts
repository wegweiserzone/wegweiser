/**
 * The keyboard, and the tokens.
 */

import { expect, reset, seed, signIn, test } from "./fixtures";

test.describe.configure({ mode: "serial" });

test.beforeAll(async ({ server }) => {
  await reset(server);
  await seed(server, "POST", "/zones", { name: "example.com" });
  await seed(server, "POST", "/zones", { name: "internal.lan" });
});

test("Ctrl+K opens the commands and Escape closes them", async ({ page, server }) => {
  await signIn(page, server);

  await page.keyboard.press("Control+k");
  await expect(page.getByRole("dialog", { name: "Commands" })).toBeVisible();

  await page.keyboard.press("Escape");
  await expect(page.getByRole("dialog", { name: "Commands" })).toHaveCount(0);
});

test("typing a zone name goes there", async ({ page, server }) => {
  await signIn(page, server);

  await page.keyboard.press("Control+k");
  await page.getByRole("textbox", { name: "Command" }).fill("internal");
  await page.keyboard.press("Enter");

  await expect(page).toHaveURL(/\/zones\/internal\.lan\.$/);
});

test("the arrow keys move and Enter runs what is under them", async ({ page, server }) => {
  await signIn(page, server);

  await page.keyboard.press("Control+k");
  await page.getByRole("textbox", { name: "Command" }).fill("stream");
  await page.keyboard.press("Enter");

  await expect(page).toHaveURL(/\/stream$/);
});

test("slash focuses whatever this screen filters by", async ({ page, server }) => {
  await signIn(page, server);
  await page.goto(`${server.url}/zones`);
  // The key is pressed once; waiting for the screen first is what makes that
  // one press land after the application has mounted.
  await expect(page.getByLabel("Search zones")).toBeVisible();

  await page.keyboard.press("/");
  await expect(page.getByLabel("Search zones")).toBeFocused();

  // And typing into it does not reopen the palette or steal the slash.
  await page.keyboard.type("example");
  await expect(page.getByLabel("Search zones")).toHaveValue("example");
});

test("a token's secret is shown once, and says so", async ({ page, server }) => {
  await signIn(page, server);
  await page.getByRole("link", { name: "Tokens" }).click();

  await page.getByRole("button", { name: "+ New token" }).click();
  await page.getByLabel("Name", { exact: true }).fill("deploy pipeline");
  await page.getByRole("button", { name: "Create token" }).click();

  const shown = page.getByRole("alert");
  await expect(shown.getByText("Copy it now; it is not shown again")).toBeVisible();
  // The credential itself, in full: the list only ever holds its prefix,
  // because the prefix is all the server keeps in the clear.
  await expect(shown.getByText(/^weg_.{20,}$/)).toBeVisible();
  await expect(page.getByRole("cell", { name: "deploy pipeline", exact: true })).toBeVisible();

  // Dismissing it is final: the server does not have the secret to show again.
  await page.getByRole("button", { name: "Done" }).click();
  await expect(page.getByRole("alert")).toHaveCount(0);
});

test("revoking keeps the token in the list", async ({ page, server }) => {
  await signIn(page, server);
  await page.goto(`${server.url}/tokens`);

  await page.getByRole("button", { name: "Revoke deploy pipeline" }).click();
  await page.getByRole("button", { name: "Revoke it" }).click();

  // Not deleted: the history points at a token to say who did something, and
  // a name that has been erased answers nothing.
  await expect(page.getByRole("cell", { name: "deploy pipeline", exact: true })).toBeVisible();
  await expect(page.getByText("Revoked")).toBeVisible();
});

test("revoking the token you are signed in with says so first", async ({ page, server }) => {
  await signIn(page, server);
  await page.goto(`${server.url}/tokens`);

  await page.getByRole("button", { name: "Revoke bootstrap" }).click();
  await expect(page.getByText("This is the token you are signed in with")).toBeVisible();
  await page.getByRole("button", { name: "Cancel" }).click();
});

// The palette lists `g o`, `g z` and the rest beside each place, so the list
// is also how anybody finds out the keys exist. A hint that does not work is
// worse than no hint.
test("g then a letter goes where the palette says it does", async ({ page, server }) => {
  await signIn(page, server);
  await expect(page.getByRole("heading", { name: "Overview" })).toBeVisible();

  await page.keyboard.press("g");
  await page.keyboard.press("z");
  await expect(page).toHaveURL(/\/zones$/);

  await page.keyboard.press("g");
  await page.keyboard.press("h");
  await expect(page).toHaveURL(/\/history$/);
});

test("a stray g does not eat the next keystroke for ever", async ({ page, server }) => {
  await signIn(page, server);
  await page.goto(`${server.url}/zones`);
  await expect(page.getByLabel("Search zones")).toBeVisible();

  await page.keyboard.press("g");
  // Long enough that the pending g has been forgotten.
  await page.waitForTimeout(1500);
  await page.keyboard.press("z");
  await expect(page).toHaveURL(/\/zones$/);

  // And typing into a field is never navigation.
  await page.getByLabel("Search zones").click();
  await page.keyboard.type("gz");
  await expect(page.getByLabel("Search zones")).toHaveValue("gz");
});
