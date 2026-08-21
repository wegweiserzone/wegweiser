/**
 * Who this browser is authenticated as.
 *
 * The interface holds no token. It posts one once and the server sets an
 * httpOnly session cookie, which no script injected into the page can read —
 * which is the entire reason this is not localStorage (docs/decisions.md D5).
 * So there is nothing to keep here but the answer to "who am I", and the
 * server is the only one who can give it.
 */

import { ApiError, NetworkError, allows, onSessionEnded, signIn, signOut, whoami } from "./api";
import type { Scope, Session } from "./api";

/** Status is what the interface should be showing. */
export type Status =
  /** Nothing has been asked yet; the first request is in flight. */
  | "checking"
  /** The server answered, and this browser is nobody. Show the token screen. */
  | "anonymous"
  /** There is a session. Show the interface. */
  | "authenticated"
  /** The server could not be reached at all, which is not the same thing. */
  | "unreachable";

let status = $state<Status>("checking");
let who = $state<Session | null>(null);
let refused = $state<string | null>(null);
let busy = $state(false);

// One place notices a session ending. Without it every open view would have to
// recognise a 401 for itself and agree on what to do about it.
onSessionEnded(() => {
  status = "anonymous";
  who = null;
});

export const session = {
  get status(): Status {
    return status;
  },

  /** who is the session, or null when there is none. */
  get who(): Session | null {
    return who;
  },

  /** refused is why the last attempt to sign in did not work. */
  get refused(): string | null {
    return refused;
  },

  /** busy is true while a sign-in is in flight. */
  get busy(): boolean {
    return busy;
  },

  /** can reports whether this session carries at least the given scope. */
  can(scope: Scope): boolean {
    return allows(who?.scopes, scope);
  },

  /**
   * check asks the server who this browser is.
   */
  async check(): Promise<void> {
    try {
      who = await whoami();
      status = "authenticated";
    } catch (err) {
      who = null;
      if (err instanceof NetworkError) {
        status = "unreachable";
        return;
      }
      status = "anonymous";
    }
  },

  /** open exchanges a token for a session. */
  async open(token: string): Promise<boolean> {
    busy = true;
    refused = null;
    try {
      who = await signIn(token.trim());
      status = "authenticated";
      return true;
    } catch (err) {
      if (err instanceof NetworkError) {
        refused = "The server could not be reached. Check that it is running and that this page is served by it.";
      } else if (err instanceof ApiError && err.status === 429) {
        refused = err.detail ?? "Too many attempts. Wait a moment and try again.";
      } else if (err instanceof ApiError && err.unauthenticated) {
        refused = "That token was not accepted. Tokens start with weg_ and are shown once, when they are created.";
      } else if (err instanceof ApiError) {
        refused = err.detail ?? err.title;
      } else {
        refused = "Something went wrong that the interface does not recognise.";
      }
      return false;
    } finally {
      busy = false;
    }
  },

  /** close ends the session here and on the server. */
  async close(): Promise<void> {
    try {
      await signOut();
    } catch {
      // The cookie is gone either way as far as this page is concerned, and
      // refusing to let somebody sign out because the network hiccuped is the
      // wrong side to fail on.
    }
    who = null;
    refused = null;
    status = "anonymous";
  },
};
