/**
 * What the client refuses to compile.
 *
 * There is nothing to run here: every `@ts-expect-error` below is the test,
 * and `npm run check` fails if one of them stops being an error. That is the
 * regression this file exists for: the typing is several conditional types
 * deep, and a wrong turn in one of them silently widens a call to `any`,
 * after which the interface compiles happily and sends nonsense.
 */

import { api } from "./index";
import type { Zone } from "./index";

/** These are correct and must stay that way. */
export async function accepted() {
  const page = await api.get("/zones", { query: { limit: 10, kind: "reverse" } });
  const zones: Zone[] = page.items;

  const made = await api.post("/zones", { body: { name: "example.com." } });
  const record = await api.get("/records/{recordId}", { path: { recordId: "r1" } });

  // No body, no path parameters, no query: none of them may be demanded.
  const health = await api.get("/healthz");

  return [zones.length, made.name, record.data, health.status];
}

/** These are mistakes, and the compiler has to say so. */
export async function refused() {
  // @ts-expect-error a path this API does not have
  await api.get("/no-such-path");

  // @ts-expect-error limit is a number
  await api.get("/zones", { query: { limit: "ten" } });

  // @ts-expect-error the field is `name`
  await api.post("/zones", { body: { nmae: "typo.example.com." } });

  // @ts-expect-error the path parameter is not optional
  await api.get("/records/{recordId}", {});

  // @ts-expect-error a page of zones is not a number
  const wrong: number = (await api.get("/zones")).items;

  // @ts-expect-error creating a zone needs a body
  await api.post("/zones");

  return wrong;
}
