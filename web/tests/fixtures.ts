/**
 * The fixtures every browser test uses.
 */

import { test as base, expect, request, type Page } from "@playwright/test";

import { started, type Server } from "./server";

interface Fixtures {
  server: Server;
  page: Page;
}

export const test = base.extend<Fixtures>({
  server: async ({}, use) => {
    await use(started());
  },

  page: async ({ page }, use) => {
    const faults: string[] = [];
    page.on("pageerror", (err) => faults.push(`uncaught: ${err.message}`));
    page.on("console", (msg) => {
      if (msg.type() === "error") faults.push(`console: ${msg.text()}`);
    });

    await use(page);

    expect(faults, "the page reported errors").toEqual([]);
  },
});

/** signIn opens a session the way a person does, through the form. */
export async function signIn(page: Page, server: Server) {
  await page.goto(server.url);
  await page.getByLabel("API token").fill(server.token);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("heading", { name: "Overview" })).toBeVisible();
}

/**
 * seed puts data in through the API rather than through the screen.
 */
export async function seed(
  server: Server,
  method: "POST" | "PATCH" | "DELETE",
  path: string,
  body?: unknown,
): Promise<Record<string, unknown>> {
  const context = await request.newContext({
    baseURL: server.url,
    extraHTTPHeaders: { Authorization: `Bearer ${server.token}` },
  });
  const response = await context.fetch(`/api/v1${path}`, { method, data: body });
  if (!response.ok()) {
    throw new Error(`seed ${method} ${path}: ${response.status()} ${await response.text()}`);
  }
  const text = await response.text();
  await context.dispose();
  return text ? (JSON.parse(text) as Record<string, unknown>) : {};
}

/**
 * reset empties the server.
 */
export async function reset(server: Server): Promise<void> {
  const context = await request.newContext({
    baseURL: server.url,
    extraHTTPHeaders: { Authorization: `Bearer ${server.token}` },
  });

  const listing = await context.fetch("/api/v1/zones?limit=1000");
  const page = (await listing.json()) as { items: { id: string }[] };
  for (const zone of page.items) {
    await context.fetch(`/api/v1/zones/${zone.id}`, { method: "DELETE" });
  }
  await context.dispose();
}

export { expect };
