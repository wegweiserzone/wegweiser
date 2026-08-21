/**
 * The record editor, and the reverse automation it is there to make visible.
 */

import { expect, reset, seed, signIn, test } from "./fixtures";

test.describe.configure({ mode: "serial" });

/** at is the record page for a zone, by apex. */
const at = (base: string, apex: string) => `${base}/zones/${encodeURIComponent(apex)}`;

// The suite shares one server, so this file says what it starts from: a
// forward zone, and the reverse zone for one of the two networks used below —
// so both "the PTR was written" and "there is no zone for it" are reachable.
test.beforeAll(async ({ server }) => {
  await reset(server);
  await seed(server, "POST", "/zones", { name: "example.com", defaultTtl: 3600 });
  await seed(server, "POST", "/zones", { name: "0.168.192.in-addr.arpa" });
});

test("the records are listed, and the apex is written once", async ({ page, server }) => {
  await signIn(page, server);
  await page.goto(at(server.url, "example.com."));

  await expect(page.getByRole("columnheader", { name: "Data" })).toBeVisible();

  // A new zone has its name server. The start of authority is deliberately not
  // in here: it belongs to the zone rather than being a record in it, which is
  // why it is on the Settings tab and why the API's listing leaves it out.
  await expect(page.getByRole("cell", { name: "NS", exact: true })).toBeVisible();
  // The apex is written as @, the way a zonefile does.
  await expect(page.getByRole("cell", { name: "@", exact: true })).toBeVisible();
  await expect(page.getByRole("cell", { name: "SOA", exact: true })).toHaveCount(0);
});

test("an address writes its PTR, and the interface says so", async ({ page, server }) => {
  await signIn(page, server);
  await page.goto(at(server.url, "example.com."));

  await page.getByRole("button", { name: "+ New record" }).click();
  await page.getByLabel("Name", { exact: true }).fill("printer");
  await page.getByLabel("Address", { exact: true }).fill("192.168.0.44");
  await page.getByRole("button", { name: "Add record" }).click();

  // Not a log line and not a toast that disappears: what the write caused is
  // on the screen, because automation nobody can see is automation being done
  // to you.
  await expect(page.getByText("44.0.168.192.in-addr.arpa.")).toBeVisible();
  await expect(page.getByText("printer.example.com.").first()).toBeVisible();
});

test("an address with no reverse zone says which one is missing", async ({ page, server }) => {
  await signIn(page, server);
  await page.goto(at(server.url, "example.com."));

  await page.getByRole("button", { name: "+ New record" }).click();
  await page.getByLabel("Name", { exact: true }).fill("offsite");
  await page.getByLabel("Address", { exact: true }).fill("203.0.113.9");
  await page.getByRole("button", { name: "Add record" }).click();

  // D6: a reverse zone is offered, never conjured.
  await expect(page.getByText("113.0.203.in-addr.arpa")).toBeVisible();
  await expect(page.getByText("No zone")).toBeVisible();
});

test("a second name for one address is refused the PTR, not the record", async ({
  page,
  server,
}) => {
  await signIn(page, server);
  await page.goto(at(server.url, "example.com."));

  await page.getByRole("button", { name: "+ New record" }).click();
  await page.getByLabel("Name", { exact: true }).fill("scanner");
  await page.getByLabel("Address", { exact: true }).fill("192.168.0.44");
  await page.getByRole("button", { name: "Add record" }).click();

  // D3: first-wins, and never silent. The A record is still written.
  await expect(page.getByText("Kept")).toBeVisible();
  await expect(page.getByText("still points at")).toBeVisible();
  await expect(page.getByRole("cell", { name: "192.168.0.44" }).first()).toBeVisible();
});

test("a generated record is not edited in place, it is taken over", async ({ page, server }) => {
  await signIn(page, server);
  await page.goto(at(server.url, "0.168.192.in-addr.arpa."));

  await page.getByRole("button", { name: /^Edit 44\./ }).click();

  // D4: the way out of the automation is to detach, and the dialog offers it
  // rather than refusing with an error after the fact.
  await expect(page.getByText("This server maintains this record")).toBeVisible();
  // Named, not identified: a ULID tells a person nothing about which record
  // they would have to change instead.
  await expect(page.getByText("printer.example.com. 3600 IN A 192.168.0.44")).toBeVisible();
  await expect(page.getByLabel("Points at", { exact: true })).toBeDisabled();

  await page.getByRole("button", { name: "Take it over" }).click();
  await expect(page.getByLabel("Points at", { exact: true })).toBeEnabled();

  await page.getByLabel("Points at", { exact: true }).fill("printer.internal.example.");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect(page.getByRole("cell", { name: "printer.internal.example." })).toBeVisible();
});

test("a record can be deleted", async ({ page, server }) => {
  await signIn(page, server);
  await page.goto(at(server.url, "example.com."));

  await page.getByRole("button", { name: /^Delete offsite\.example\.com\. A$/ }).click();
  await page.getByRole("button", { name: "Delete record" }).click();

  await expect(page.getByRole("cell", { name: "203.0.113.9" })).toHaveCount(0);
});

// The search is the server's, so it reaches records this page never loaded.
// A filter over the loaded page would be a filter that lies as soon as a zone
// is bigger than one page.
test("the search narrows the whole zone, not the page", async ({ page, server }) => {
  await signIn(page, server);
  await page.goto(at(server.url, "example.com."));

  const search = page.getByLabel("Search records");
  await search.fill("192.168.0.44");
  await expect(page.getByRole("cell", { name: "192.168.0.44" }).first()).toBeVisible();
  await expect(page.getByRole("cell", { name: "NS", exact: true })).toHaveCount(0);

  await search.fill("nothing-like-this");
  await expect(page.getByRole("heading", { name: "Nothing matches" })).toBeVisible();
});

test("the type filter is the server's too", async ({ page, server }) => {
  await signIn(page, server);
  await page.goto(at(server.url, "example.com."));

  await page.getByLabel("Type").selectOption("A");
  await expect(page.getByRole("cell", { name: "A", exact: true }).first()).toBeVisible();
  await expect(page.getByRole("cell", { name: "NS", exact: true })).toHaveCount(0);
});

// A page is a page: the size is a real limit and Next is real navigation.
test("a listing pages", async ({ page, server }) => {
  await signIn(page, server);
  await page.goto(at(server.url, "example.com."));

  await page.getByLabel("per page").selectOption("100");
  await expect(page.getByText("page 1", { exact: true })).toBeVisible();
  // This zone is small, so there is nothing after the first page and Next
  // says so rather than pretending.
  await expect(page.getByRole("button", { name: "Next" })).toBeDisabled();
  await expect(page.getByRole("button", { name: "Previous" })).toBeDisabled();
});

// A name server inside the zone it serves needs an address in that zone, or a
// resolver referred to it is told the name does not exist. Nothing else about
// the zone looks wrong, which is what makes it worth saying out loud.
test("a name server with no address is called out", async ({ page, server }) => {
  await signIn(page, server);
  await page.goto(at(server.url, "example.com."));

  await expect(page.getByText("A name server here has no address")).toBeVisible();
  await expect(page.getByText("ns1.example.com.").first()).toBeVisible();

  // And it can be fixed from where it is said.
  await page.getByRole("button", { name: "Add its address" }).click();
  await expect(page.getByLabel("Name", { exact: true })).toHaveValue("ns1");
  await page.getByLabel("Address", { exact: true }).fill("192.168.0.2");
  await page.getByRole("button", { name: "Add record" }).click();

  await expect(page.getByText("A name server here has no address")).toHaveCount(0);
});

// "10 60 5060 pbx.example.com." is four things with names, and asking somebody
// to remember the order is asking them to learn zonefile syntax: the one
// thing this product promises they will not have to.
test("a record's data is the parts it is made of", async ({ page, server }) => {
  await signIn(page, server);
  await page.goto(at(server.url, "example.com."));

  await page.getByRole("button", { name: "+ New record" }).click();
  await page.getByLabel("Name", { exact: true }).fill("_sip._tcp");
  await page.getByRole("combobox", { name: "Type", exact: true }).fill("SRV");

  // Four fields with names, not one box wanting an order.
  await page.getByLabel("Priority").fill("10");
  await page.getByLabel("Weight").fill("60");
  await page.getByLabel("Port").fill("5060");
  await page.getByLabel("Target").fill("pbx.example.com.");

  // And it shows what it is assembling, so the syntax is learnable rather
  // than hidden.
  await expect(page.getByText("10 60 5060 pbx.example.com.")).toBeVisible();

  await page.getByRole("button", { name: "Add record" }).click();
  await expect(page.getByRole("cell", { name: "10 60 5060 pbx.example.com." })).toBeVisible();
});

test("quoting is added rather than asked for", async ({ page, server }) => {
  await signIn(page, server);
  await page.goto(at(server.url, "example.com."));

  await page.getByRole("button", { name: "+ New record" }).click();
  await page.getByLabel("Name", { exact: true }).fill("_dmarc");
  await page.getByRole("combobox", { name: "Type", exact: true }).fill("TXT");
  await page.getByLabel("Text").fill("v=DMARC1; p=none");

  await page.getByRole("button", { name: "Add record" }).click();
  // Stored as a character-string of RFC 1035 §3.3.14, which nobody had to type.
  await expect(page.getByRole("cell", { name: '"v=DMARC1; p=none"' })).toBeVisible();
});

test("editing an existing record opens it as its parts", async ({ page, server }) => {
  await signIn(page, server);
  await page.goto(at(server.url, "example.com."));

  await page.getByRole("button", { name: /^Edit _sip\._tcp\.example\.com\. SRV$/ }).click();

  await expect(page.getByLabel("Priority")).toHaveValue("10");
  await expect(page.getByLabel("Port")).toHaveValue("5060");
  await expect(page.getByLabel("Target")).toHaveValue("pbx.example.com.");

  await page.getByLabel("Port").fill("5061");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect(page.getByRole("cell", { name: "10 60 5061 pbx.example.com." })).toBeVisible();
});

// A type this interface has never heard of still works, which is the case that
// must not break: the server stores anything, including TYPE<number>.
test("an unknown type falls back to one box", async ({ page, server }) => {
  await signIn(page, server);
  await page.goto(at(server.url, "example.com."));

  await page.getByRole("button", { name: "+ New record" }).click();
  await page.getByLabel("Name", { exact: true }).fill("odd");
  await page.getByRole("combobox", { name: "Type", exact: true }).fill("TYPE65534");
  await page.getByLabel("Data", { exact: true }).fill("\\# 3 010203");

  await page.getByRole("button", { name: "Add record" }).click();
  await expect(page.getByRole("cell", { name: "\\# 3 010203" })).toBeVisible();
});
