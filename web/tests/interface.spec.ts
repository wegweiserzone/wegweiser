/**
 * That the interface is there, and reaches the server.
 */

import { expect, signIn, test } from "./fixtures";

test("the session screen says which server this is", async ({ page, server }) => {
  await page.goto(server.url);

  await expect(page.getByText("Wegweiser").first()).toBeVisible();
  // Read from /healthz, which needs no credential: "which server am I typing
  // my token into" is answerable before anybody authenticates.
  await expect(page.getByText("Serving")).toBeVisible();
  await expect(page.getByLabel("API token")).toBeVisible();
});

test("a token that is not one is refused with something to do about it", async ({
  page,
  server,
}) => {
  await page.goto(server.url);
  await page.getByLabel("API token").fill("weg_not-a-real-token");
  await page.getByRole("button", { name: "Sign in" }).click();

  const refusal = page.getByRole("alert");
  await expect(refusal).toBeVisible();
  await expect(refusal).toContainText("weg_");
  // Still on the screen that can fix it.
  await expect(page.getByLabel("API token")).toBeVisible();
});

test("signing in shows the shell reading a live server", async ({ page, server }) => {
  await signIn(page, server);

  // Numbers from /healthz, not from the interface's imagination.
  await expect(page.getByText("serving")).toBeVisible();
  // The session is the one the token opened.
  await expect(page.getByText("bootstrap")).toHaveCount(2); // rail and overview
  await expect(page.getByText("admin").first()).toBeVisible();
});

// The counters come from the server's own metrics, which is the only place
// they exist: the API reports what is there, not what has been asked.
test("the overview reads what the server has answered", async ({ page, server }) => {
  await signIn(page, server);

  await expect(page.getByRole("heading", { name: "Answers" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Questions" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Latency" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Recent changes" })).toBeVisible();

  // Uptime is derived from process_start_time_seconds, so a number here means
  // the exposition format was parsed rather than skipped.
  await expect(page.getByText(/^\d+[dhm]/)).toBeVisible();
});

// Every section in the rail leads somewhere. There is no "soon" any more:
// the last of them was built, and a navigation item that goes nowhere would be
// the interface promising something it cannot do.
test("every section in the rail leads somewhere", async ({ page, server }) => {
  await signIn(page, server);

  const sections = page.getByLabel("Sections").getByRole("link");
  await expect(sections).toHaveCount(5);
  for (const name of ["Overview", "Zones", "Query stream", "History", "Tokens"]) {
    await expect(page.getByLabel("Sections").getByRole("link", { name })).toBeVisible();
  }
  await expect(page.getByText("soon")).toHaveCount(0);
});

test("the theme survives a reload", async ({ page, server }) => {
  await signIn(page, server);

  const root = page.locator("html");
  await expect(root).not.toHaveAttribute("data-theme", "light");

  await page.getByRole("button", { name: "Switch theme" }).click();
  await expect(root).toHaveAttribute("data-theme", "light");

  // The choice is applied by a blocking script before the first paint. If that
  // ever stops working, every load flashes the wrong colours, which is
  // invisible in a screenshot taken afterwards and visible here.
  await page.reload();
  await expect(root).toHaveAttribute("data-theme", "light");
});

test("signing out ends the session", async ({ page, server }) => {
  await signIn(page, server);

  await page.getByRole("button", { name: "Sign out" }).click();
  await expect(page.getByLabel("API token")).toBeVisible();

  // And it is really gone, not merely hidden: a reload does not walk back in.
  await page.reload();
  await expect(page.getByLabel("API token")).toBeVisible();
});

test("a page that does not exist is designed, not SvelteKit's", async ({ page, server }) => {
  // Typed into the address bar, not navigated to from inside: the fallback
  // document is served (internal/api/ui.go) and the client-side router then
  // finds it has no such page.
  const response = await page.goto(`${server.url}/somewhere/deep`);
  expect(response?.status()).toBe(200);

  await expect(page.getByRole("heading", { name: "No such page" })).toBeVisible();
  // It names the path, because a typed or stale address is the usual cause and
  // seeing it is half the answer.
  await expect(page.getByText("/somewhere/deep")).toBeVisible();
  // And it is not the framework's bare <h1>404</h1>.
  await expect(page.getByRole("heading", { name: "404", exact: true })).toHaveCount(0);
});

test("the design system renders", async ({ page, server }) => {
  // Outside the session gate on purpose: it is a reference for whoever is
  // building the interface, not part of the product.
  await page.goto(`${server.url}/design`);

  await expect(page.getByRole("heading", { name: "Colour" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Type" })).toBeVisible();
});
