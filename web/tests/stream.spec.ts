/**
 * The live query tail.
 */

import { spawnSync } from "node:child_process";

import { expect, reset, seed, signIn, test } from "./fixtures";
import { started } from "./server";

test.describe.configure({ mode: "serial" });

test.beforeAll(async ({ server }) => {
  await reset(server);
  await seed(server, "POST", "/zones", { name: "example.com" });
});

/** ask sends one query to the server's own DNS port, if dig is here. */
function ask(name: string, type = "A"): boolean {
  const { dns } = started();
  const [host, port] = [dns.slice(0, dns.lastIndexOf(":")), dns.slice(dns.lastIndexOf(":") + 1)];
  const out = spawnSync("dig", ["+tries=1", "+time=1", `@${host}`, "-p", port, name, type], {
    encoding: "utf8",
  });
  return out.status === 0;
}

test("the stream says it is live before anything is asked", async ({ page, server }) => {
  await signIn(page, server);
  await page.getByRole("link", { name: "Query stream" }).click();

  await expect(page.getByRole("heading", { name: "Query stream" })).toBeVisible();
  await expect(page.getByText("Live", { exact: true })).toBeVisible();

  // An idle server is not an error, and the empty state says what to do about
  // it rather than leaving a blank table.
  await expect(page.getByRole("heading", { name: "Nothing is being asked" })).toBeVisible();
});

test("a query appears as it is answered", async ({ page, server }) => {
  test.skip(!ask("probe.example.com"), "dig is not installed");

  await signIn(page, server);
  await page.goto(`${server.url}/stream`);
  await expect(page.getByText("Live", { exact: true })).toBeVisible();

  ask("www.example.com");

  await expect(page.getByRole("cell", { name: "www.example.com." })).toBeVisible();
  // NOERROR is the zone's own name; the server answers it authoritatively.
  await expect(page.getByText("NOERROR").first()).toBeVisible();
});

test("a name this server does not hold is shown as refused", async ({ page, server }) => {
  test.skip(!ask("probe.example.com"), "dig is not installed");

  await signIn(page, server);
  await page.goto(`${server.url}/stream`);
  await expect(page.getByText("Live", { exact: true })).toBeVisible();

  ask("somewhere.else.invalid");

  await expect(page.getByText("REFUSED").first()).toBeVisible();
});

test("the filter is the server's, so changing it reopens the stream", async ({
  page,
  server,
}) => {
  test.skip(!ask("probe.example.com"), "dig is not installed");

  await signIn(page, server);
  await page.goto(`${server.url}/stream`);
  await expect(page.getByText("Live", { exact: true })).toBeVisible();

  await page.getByLabel("Watch a name and everything below it").fill("example.com");
  // The rows collected under the old filter are gone: this is a live view of
  // one thing, not a search over what was collected under another.
  await expect(page.getByRole("heading", { name: "Nothing is being asked" })).toBeVisible();

  ask("filtered.example.com");
  await expect(page.getByRole("cell", { name: "filtered.example.com." })).toBeVisible();

  ask("outside.invalid");
  await expect(page.getByRole("cell", { name: "outside.invalid." })).toHaveCount(0);
});

test("pausing holds the table and says what was missed", async ({ page, server }) => {
  test.skip(!ask("probe.example.com"), "dig is not installed");

  await signIn(page, server);
  await page.goto(`${server.url}/stream`);
  await expect(page.getByText("Live", { exact: true })).toBeVisible();

  ask("before.example.com");
  await expect(page.getByRole("cell", { name: "before.example.com." })).toBeVisible();

  await page.getByRole("button", { name: "Pause" }).click();
  await expect(page.getByText("Paused")).toBeVisible();

  ask("during.example.com");
  await expect(page.getByText("arrived while paused")).toBeVisible();
  // The table did not move.
  await expect(page.getByRole("cell", { name: "during.example.com." })).toHaveCount(0);

  await page.getByRole("button", { name: "Resume" }).click();
  await expect(page.getByText("Live", { exact: true })).toBeVisible();
});
