/**
 * Managing the identity providers in the interface: who each lets in, grouped
 * by scope, and how the server reaches it
 * (docs/decisions/d51-accounts-come-from-an-identity-provider.md).
 *
 * Every provider is removed after each test, because every other file signs in
 * on the same server and expects the token form alone.
 */

import { request } from "@playwright/test";

import { expect, signIn, test } from "./fixtures";
import type { Server } from "./server";

async function removeProviders(server: Server) {
  const context = await request.newContext({
    baseURL: server.url,
    extraHTTPHeaders: { Authorization: `Bearer ${server.token}` },
  });
  const listing = await context.fetch("/api/v1/identity-providers");
  for (const p of (await listing.json()) as { id: string }[]) {
    await context.fetch(`/api/v1/identity-providers/${p.id}`, { method: "DELETE" });
  }
  await context.dispose();
}

test.afterEach(async ({ server }) => {
  await removeProviders(server);
});

test("an OpenID Connect provider is added, given groups by scope, switched off and removed", async ({
  page,
  server,
}) => {
  await signIn(page, server);
  await page.goto(`${server.url}/providers`);
  await expect(page.getByText("No providers yet")).toBeVisible();
  await page.getByRole("button", { name: "Add OpenID Connect" }).click();

  await page.getByLabel("Name", { exact: true }).fill("Authentik");
  await page.getByLabel("Issuer").fill("https://127.0.0.1:9/application/o/weg/");
  await page.getByLabel("Client ID").fill("weg");
  await page.getByLabel("Client secret").fill("s3cret");
  // The callback on the address the page is served from is offered, since it
  // is the one most often forgotten.
  await page.getByRole("button", { name: "Add it" }).click();
  await expect(page.getByRole("list", { name: "Redirect URIs" })).toContainText(
    `${server.url}/api/v1/auth/oidc/callback`,
  );

  // Nobody is let in until a group is given a scope, and the page says so.
  await expect(page.getByText("Nobody can sign in through it yet")).toBeVisible();
  const given: [string, string][] = [
    ["admin", "dns-admins"],
    ["write", "dns-ops"],
    ["read", "dns-readers"],
  ];
  for (const [scope, group] of given) {
    await page.getByLabel(`Add to Groups given ${scope}`).fill(group);
    await page.getByLabel(`Add to Groups given ${scope}`).press("Enter");
  }
  await expect(page.getByText("Nobody can sign in through it yet")).toHaveCount(0);
  await page.getByRole("button", { name: "Remove dns-readers" }).click();

  await page.getByRole("button", { name: "Add provider" }).click();
  await expect(page.getByText("Added.")).toBeVisible();
  await expect(page).toHaveURL(/\/providers\/[0-9A-Z]{26}$/);

  // What was saved is what comes back, and the secret does not.
  await page.reload();
  await expect(page.getByRole("list", { name: "Groups given admin" })).toContainText("dns-admins");
  await expect(page.getByRole("list", { name: "Groups given write" })).toContainText("dns-ops");
  await expect(page.getByRole("list", { name: "Groups given read" })).toContainText(
    "No group is given read.",
  );
  await expect(page.getByLabel("Client secret")).toHaveValue("");
  await expect(page.getByRole("button", { name: "Save" })).toBeDisabled();

  // The sign-in page is told about it.
  const offered = await page.request.get(`${server.url}/api/v1/auth/providers`);
  expect(((await offered.json()) as { name: string }[]).map((p) => p.name)).toEqual(["Authentik"]);

  await page.goto(`${server.url}/providers`);
  const row = page.getByRole("row", { name: /Authentik/ });
  await expect(row).toContainText("1 admin");
  await expect(row).toContainText("1 write");
  await expect(row).toContainText("Offered");

  // Switched off, it stays in the list and leaves the sign-in page.
  await row.getByRole("link", { name: "Authentik" }).click();
  await page.getByLabel("Offered on the sign-in page").uncheck();
  await page.getByRole("button", { name: "Save" }).click();
  await expect(page.getByText("Saved. A session already open")).toBeVisible();
  await page.goto(`${server.url}/providers`);
  await expect(page.getByRole("row", { name: /Authentik/ })).toContainText("Off");

  await page.getByRole("link", { name: "Authentik" }).click();
  await page.getByRole("button", { name: "Remove Authentik" }).click();
  await page.getByRole("button", { name: "Remove it" }).click();
  await expect(page).toHaveURL(/\/providers$/);
  await expect(page.getByText("No providers yet")).toBeVisible();
});

test("a directory keeps its bind password to itself", async ({ page, server }) => {
  await signIn(page, server);
  await page.goto(`${server.url}/providers/new?kind=ldap`);

  await page.getByLabel("Name", { exact: true }).fill("FreeIPA");
  await page.getByLabel("URL", { exact: true }).fill("ldaps://127.0.0.1:9");
  await page.getByLabel("Bind DN").fill("uid=wegweiser,cn=sysaccounts,cn=etc,dc=example,dc=com");
  await page.getByLabel("Bind password").fill("hunter2");
  await page.getByLabel("User base").fill("cn=users,cn=accounts,dc=example,dc=com");
  await page.getByLabel("Add to Groups given admin").fill("admins");
  await page.getByLabel("Add to Groups given admin").press("Enter");

  // A search asks for where to search, and memberOf does not.
  await expect(page.getByLabel("Group base")).toHaveCount(0);
  await page.getByText("A search for groups naming them").click();
  await page.getByLabel("Group base").fill("cn=groups,cn=accounts,dc=example,dc=com");

  await page.getByRole("button", { name: "Add provider" }).click();
  await expect(page.getByText("Added.")).toBeVisible();

  await page.reload();
  await expect(page.getByLabel("Bind password")).toHaveValue("");
  await expect(page.getByLabel("Group base")).toHaveValue("cn=groups,cn=accounts,dc=example,dc=com");
  await expect(page.locator("body")).not.toContainText("hunter2");

  // A change that leaves the password out keeps it: the provider is still
  // one the server will search with.
  await page.getByLabel("Name", { exact: true }).fill("IPA");
  await page.getByRole("button", { name: "Save" }).click();
  await expect(page.getByText("Saved. A session already open")).toBeVisible();
});
