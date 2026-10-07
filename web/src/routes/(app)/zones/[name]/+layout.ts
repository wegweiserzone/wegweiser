import { error } from "@sveltejs/kit";

import { api, ApiError, NetworkError } from "$lib/api";
import { nearest } from "$lib/nearest";

import type { LayoutLoad } from "./$types";

/**
 * The zone every page under here is about.
 */
export const load: LayoutLoad = async ({ params }) => {
  const apex = params.name;

  try {
    const answer = await api.get("/zones", { query: { name: apex, limit: 1 } });
    const zone = answer.items[0];
    if (!zone) {
      // A typed or stale address is the usual way here, so the zone it most
      // likely meant is worth one more request.
      const held = await api.get("/zones", { query: { limit: 1000 } });
      error(404, {
        message: `There is no zone ${apex} on this server.`,
        zone: apex,
        nearest: nearest(apex.endsWith(".") ? apex : `${apex}.`, held.items.map((z) => z.name)),
      });
    }
    return { zone };
  } catch (err) {
    if (err instanceof NetworkError) {
      error(503, "The server did not answer.");
    }
    if (err instanceof ApiError) {
      error(err.status, err.detail ?? err.title);
    }
    throw err;
  }
};
