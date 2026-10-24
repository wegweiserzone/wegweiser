/**
 * What the interface does with an error it did not expect.
 *
 * Every error comes through here, the ones the interface raises on purpose
 * with `error()` included, so only what nobody raised on purpose is reported:
 * an error thrown by code, and one SvelteKit made other than a page that is
 * not there. What the error page shows is left as SvelteKit would show it.
 */

import type { HandleClientError } from "@sveltejs/kit/hooks";

export const handleError: HandleClientError = ({ kind, error }) => {
  if (kind === "unknown" || (kind === "framework" && error.status !== 404)) {
    console.error("wegweiser: the interface failed to render", error);
  }
};
