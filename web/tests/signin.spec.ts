/**
 * Signing in through an identity provider, as far as a browser test can go
 * without one: the providers are offered, and a provider that cannot be
 * reached says so on the sign-in page rather than leaving the browser
 * somewhere else (docs/decisions/d51-accounts-come-from-an-identity-provider.md).
 *
 * The providers are made for the test and removed after it, because every
 * other file signs in on the same server and expects the token form alone.
 */

import { expect, seed, test } from "./fixtures";

// Loopback with nothing listening, so that the server's attempt to reach the
// provider fails at once rather than after a timeout.
const nowhere = "127.0.0.1:9";

test("a provider is offered, and one that cannot be reached says so", async ({ page, server }) => {
  const oidc = await seed(server, "POST", "/identity-providers", {
    name: "Nowhere SSO",
    kind: "oidc",
    oidc: {
      issuer: `https://${nowhere}/`,
      clientId: "weg",
      clientSecret: "s3cret",
      redirectUris: [`${server.url}/api/v1/auth/oidc/callback`],
    },
  });
  const ldap = await seed(server, "POST", "/identity-providers", {
    name: "Nowhere LDAP",
    kind: "ldap",
    ldap: { url: `ldaps://${nowhere}`, userBase: "dc=example,dc=com", userFilter: "(uid={username})" },
  });

  try {
    await page.goto(server.url);
    await expect(page.getByLabel("API token")).toBeVisible();

    // A directory is a way of filling the same form in.
    await page.getByRole("button", { name: "Nowhere LDAP" }).click();
    await expect(page.getByLabel("API token")).toBeHidden();
    await page.getByLabel("Username").fill("alice");
    await page.getByLabel("Password").fill("correct horse");
    await page.getByRole("button", { name: "Sign in", exact: true }).click();
    await expect(page.getByRole("alert")).toContainText("could not be asked");
    await expect(page.getByLabel("Password")).toHaveValue("");

    // An OpenID Connect provider is a navigation, and comes back here.
    await page.getByRole("button", { name: "Sign in with Nowhere SSO" }).click();
    await expect(page.getByRole("alert")).toContainText("could not be reached");
    await expect(page).not.toHaveURL(/signin_error/);
  } finally {
    await seed(server, "DELETE", `/identity-providers/${oidc.id as string}`);
    await seed(server, "DELETE", `/identity-providers/${ldap.id as string}`);
  }
});
