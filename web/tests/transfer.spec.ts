/**
 * Zonefile import and export from the interface.
 *
 * They existed in the API and the CLI and nowhere here, which is architecture
 * invariant 1 broken: no feature exists in only one client.
 */

import { expect, reset, seed, signIn, test } from "./fixtures";

test.describe.configure({ mode: "serial" });

test.beforeAll(async ({ server }) => {
  await reset(server);
});

const zonefile = `$ORIGIN imported.example.
$TTL 3600
@   IN SOA ns1.imported.example. hostmaster.imported.example. 7 3600 600 604800 300
@   IN NS  ns1.imported.example.
ns1 IN A   192.0.2.53
www IN A   192.0.2.10
`;

test("a zonefile can be brought in, and says what it did", async ({ page, server }) => {
  await signIn(page, server);
  await page.getByRole("link", { name: "Zones" }).click();

  await page.getByRole("button", { name: "Import zonefile" }).click();
  await page
    .getByLabel("Zonefile")
    .setInputFiles({ name: "imported.example.zone", mimeType: "text/dns", buffer: Buffer.from(zonefile) });
  await page.getByRole("button", { name: "Import", exact: true }).click();

  await expect(page.getByText("imported.example. is answering")).toBeVisible();

  await page.getByRole("button", { name: "Open the zone" }).click();
  await expect(page).toHaveURL(/\/zones\/imported\.example\.$/);
  // The serial the file carried is the serial it starts at (D2), not 1.
  await expect(page.getByText("7", { exact: true })).toBeVisible();
});

test("a zone can be written back out", async ({ page, server }) => {
  await seed(server, "POST", "/zones", { name: "exported.example" });
  await signIn(page, server);
  await page.goto(`${server.url}/zones/exported.example.`);

  const download = page.waitForEvent("download");
  await page.getByRole("button", { name: "Export", exact: true }).click();
  const file = await download;

  expect(file.suggestedFilename()).toBe("exported.example.zone");

  const stream = await file.createReadStream();
  const chunks: Buffer[] = [];
  for await (const chunk of stream) chunks.push(chunk as Buffer);
  const text = Buffer.concat(chunks).toString();

  // What comes out is a zonefile, with every name absolute so no line means
  // something different once it is copied out of the file.
  expect(text).toContain("exported.example.");
  expect(text).toContain("SOA");
});
