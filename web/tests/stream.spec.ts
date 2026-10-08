/**
 * The live query tail.
 */

import { createSocket } from "node:dgram";

import { expect, reset, seed, signIn, test } from "./fixtures";
import { started } from "./server";

test.describe.configure({ mode: "serial" });

test.beforeAll(async ({ server }) => {
  await reset(server);
  await seed(server, "POST", "/zones", { name: "example.com" });
});

/**
 * ask sends one query for an address to the server's own DNS port and waits
 * for the answer. Written out here rather than handed to dig, so that the
 * stream is tested wherever the suite runs, not only where dig is installed.
 */
async function ask(name: string): Promise<void> {
  const { dns } = started();
  const at = dns.lastIndexOf(":");
  const [host, port] = [dns.slice(0, at), Number(dns.slice(at + 1))];

  const labels = name
    .split(".")
    .filter(Boolean)
    .map((label) => Buffer.concat([Buffer.from([label.length]), Buffer.from(label)]));
  const query = Buffer.concat([
    // An identifier, recursion desired, one question.
    Buffer.from([0x57, 0x47, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0]),
    ...labels,
    // The root, then type A in class IN.
    Buffer.from([0, 0, 1, 0, 1]),
  ]);

  const socket = createSocket("udp4");
  try {
    await new Promise<void>((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error(`${dns} did not answer ${name}`)), 2000);
      socket.once("message", () => {
        clearTimeout(timer);
        resolve();
      });
      socket.send(query, port, host);
    });
  } finally {
    socket.close();
  }
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
  await signIn(page, server);
  await page.goto(`${server.url}/stream`);
  await expect(page.getByText("Live", { exact: true })).toBeVisible();

  await ask("www.example.com");

  await expect(page.getByRole("cell", { name: "www.example.com." })).toBeVisible();
  // NOERROR is the zone's own name; the server answers it authoritatively.
  await expect(page.getByText("NOERROR").first()).toBeVisible();
});

test("a name this server does not hold is shown as refused", async ({ page, server }) => {
  await signIn(page, server);
  await page.goto(`${server.url}/stream`);
  await expect(page.getByText("Live", { exact: true })).toBeVisible();

  await ask("somewhere.else.invalid");

  await expect(page.getByText("REFUSED").first()).toBeVisible();
});

test("the filter is the server's, so changing it reopens the stream", async ({ page, server }) => {
  await signIn(page, server);
  await page.goto(`${server.url}/stream`);
  await expect(page.getByText("Live", { exact: true })).toBeVisible();

  await page.getByLabel("Watch a name and everything below it").fill("example.com");
  // The rows collected under the old filter are gone: this is a live view of
  // one thing, not a search over what was collected under another.
  await expect(page.getByRole("heading", { name: "Nothing is being asked" })).toBeVisible();

  await ask("filtered.example.com");
  await expect(page.getByRole("cell", { name: "filtered.example.com." })).toBeVisible();

  await ask("outside.invalid");
  await expect(page.getByRole("cell", { name: "outside.invalid." })).toHaveCount(0);
});

test("pausing holds the table and says what was missed", async ({ page, server }) => {
  await signIn(page, server);
  await page.goto(`${server.url}/stream`);
  await expect(page.getByText("Live", { exact: true })).toBeVisible();

  await ask("before.example.com");
  await expect(page.getByRole("cell", { name: "before.example.com." })).toBeVisible();

  await page.getByRole("button", { name: "Pause" }).click();
  await expect(page.getByText("Paused")).toBeVisible();

  await ask("during.example.com");
  await expect(page.getByText("arrived while paused")).toBeVisible();
  // The table did not move.
  await expect(page.getByRole("cell", { name: "during.example.com." })).toHaveCount(0);

  await page.getByRole("button", { name: "Resume" }).click();
  await expect(page.getByText("Live", { exact: true })).toBeVisible();
});
