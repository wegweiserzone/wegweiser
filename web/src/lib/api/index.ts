/**
 * The API, as the rest of the interface sees it.
 */

import { ApiError, Client, NetworkError } from "./client";
import type { components } from "./schema";

export { ApiError, NetworkError, basePath } from "./client";
export type { Problem, Scope, Session } from "./client";

/** The shapes the interface works with, named without the generated wrapping. */
export type Zone = components["schemas"]["Zone"];
export type ZoneKind = components["schemas"]["ZoneKind"];
export type Record_ = components["schemas"]["Record"];
export type RRset = components["schemas"]["RRset"];
export type Commit = components["schemas"]["Commit"];
export type CommitKind = components["schemas"]["CommitKind"];
export type Event = components["schemas"]["Event"];
export type Token = components["schemas"]["Token"];
export type TSIGKey = components["schemas"]["TSIGKey"];
export type TSIGAlgorithm = components["schemas"]["TSIGAlgorithm"];
export type Health = components["schemas"]["Health"];
export type QueryEvent = components["schemas"]["QueryEvent"];
export type StreamStatus = components["schemas"]["StreamStatus"];
export type Conflict = components["schemas"]["Conflict"];
export type MissingZone = components["schemas"]["MissingZone"];
export type Settings = components["schemas"]["Settings"];
export type ZoneImported = components["schemas"]["ZoneImported"];
export type ReverseConflictPolicy = components["schemas"]["ReverseConflictPolicy"];

/**
 * onUnauthenticated is what the interface does when a session has ended.
 */
let unauthenticated: (() => void) | undefined;

/** onSessionEnded registers the one place that reacts to a lost session. */
export function onSessionEnded(handler: () => void) {
  unauthenticated = handler;
}

/** api is the client every view calls. */
export const api = new Client({
  onUnauthenticated: () => unauthenticated?.(),
});

/* ── The session ──────────────────────────────────────────────────────────
 *
 * The interface holds no token. It posts one here once and the server sets an
 * httpOnly cookie, which no script injected into the page can read, which is
 * the entire reason this is not localStorage (docs/decisions/ D5).
 */

/** signIn exchanges a token for a browser session. */
export function signIn(token: string) {
  return api.post("/auth/session", { body: { token } });
}

/** whoami reports who this browser is authenticated as, if anybody. */
export function whoami() {
  return api.get("/auth/session");
}

/** signOut ends the session here and on the server. */
export function signOut() {
  return api.delete("/auth/session");
}

/**
 * allows reports whether a session may do something.
 */
export function allows(
  scopes: components["schemas"]["Scope"][] | undefined,
  needed: components["schemas"]["Scope"],
): boolean {
  const rank = { read: 1, write: 2, admin: 3 } as const;
  const held = scopes?.reduce((best, s) => Math.max(best, rank[s] ?? 0), 0) ?? 0;
  return held >= rank[needed];
}
