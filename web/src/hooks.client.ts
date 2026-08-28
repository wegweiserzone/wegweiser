/**
 * What the interface does with an error it did not expect.
 */

import type { HandleClientError } from "@sveltejs/kit";

export const handleError: HandleClientError = ({ error, status, message }) => {
  if (status !== 404) {
    console.error("wegweiser: the interface failed to render", error);
  }
  return { message };
};
