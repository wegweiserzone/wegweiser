import { error } from "@sveltejs/kit";

import { api, ApiError, NetworkError } from "$lib/api";

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
      error(404, `This server is not authoritative for ${apex}.`);
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
