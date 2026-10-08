import { error } from "@sveltejs/kit";

import type { PageLoad } from "./$types";

/**
 * Any address the interface has no page for. Caught here rather than left to
 * the router, so that the page saying so keeps the rail: somebody who followed
 * a stale link is one click from where they meant to go, not on a blank screen.
 */
export const load: PageLoad = () => {
  error(404, "No such page");
};
