/**
 * The live query tail.
 */

import { ApiError, NetworkError, basePath } from "./api";
import type { Problem, QueryEvent, StreamStatus } from "./api";

/** Filter is what the server narrows the stream to, before it buffers anything. */
export interface Filter {
  /** A name and everything below it, case-insensitively (RFC 4343). */
  name?: string;
  /** Question types to watch. Empty watches all of them. */
  types?: string[];
  /** Response codes to watch, by mnemonic. Empty watches all of them. */
  rcodes?: string[];
  /** One address, or a network in CIDR notation. */
  client?: string;
}

/** Handlers are what a watcher does with what arrives. */
export interface Handlers {
  onOpen?: () => void;
  onQuery: (event: QueryEvent) => void;
  onStatus: (status: StreamStatus) => void;
  /** Called once, when the stream ends for a reason worth showing. */
  onError: (error: Error) => void;
}

/** watchQueries opens a stream and returns the function that closes it. */
export function watchQueries(filter: Filter, handlers: Handlers): () => void {
  const controller = new AbortController();
  void run(filter, handlers, controller.signal);
  return () => controller.abort();
}

async function run(filter: Filter, handlers: Handlers, signal: AbortSignal) {
  const url = new URL(`${basePath}/queries/stream`, location.origin);
  if (filter.name?.trim()) url.searchParams.set("name", filter.name.trim());
  if (filter.client?.trim()) url.searchParams.set("client", filter.client.trim());
  for (const type of filter.types ?? []) url.searchParams.append("type", type);
  for (const rcode of filter.rcodes ?? []) url.searchParams.append("rcode", rcode);

  let response: Response;
  try {
    response = await fetch(url, {
      headers: { Accept: "text/event-stream" },
      credentials: "same-origin",
      signal,
    });
  } catch (cause) {
    if (!signal.aborted) handlers.onError(new NetworkError(cause));
    return;
  }

  if (!response.ok || !response.body) {
    let problem: Problem | undefined;
    try {
      problem = (await response.json()) as Problem;
    } catch {
      // Not one of ours, which the ApiError below says plainly enough.
    }
    handlers.onError(
      new ApiError(
        problem?.title
          ? problem
          : {
              type: "/problems/unknown",
              title: `The server answered ${response.status}`,
              status: response.status,
            },
      ),
    );
    return;
  }

  handlers.onOpen?.();

  const reader = response.body.pipeThrough(new TextDecoderStream()).getReader();
  // Frames are separated by a blank line and may arrive split across chunks,
  // so what is left over stays here until the rest of it turns up.
  let pending = "";

  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;

      pending += value;
      let boundary = pending.indexOf("\n\n");
      while (boundary >= 0) {
        deliver(pending.slice(0, boundary), handlers);
        pending = pending.slice(boundary + 2);
        boundary = pending.indexOf("\n\n");
      }
    }
  } catch (cause) {
    if (!signal.aborted) handlers.onError(new NetworkError(cause));
    return;
  }

  // The body ended without anybody asking it to.
  if (!signal.aborted) {
    handlers.onError(new Error("The server closed the stream."));
  }
}

/** deliver turns one frame into the call it stands for. */
function deliver(frame: string, handlers: Handlers) {
  let name = "message";
  let data = "";

  for (const line of frame.split("\n")) {
    if (line.startsWith(":")) continue; // a comment, which SSE allows
    const colon = line.indexOf(":");
    if (colon < 0) continue;
    const field = line.slice(0, colon);
    const value = line.slice(colon + 1).trimStart();
    if (field === "event") name = value;
    if (field === "data") data = data ? `${data}\n${value}` : value;
  }
  if (!data) return;

  let payload: unknown;
  try {
    payload = JSON.parse(data);
  } catch {
    return; // a frame we cannot read is not a reason to end the stream
  }

  if (name === "query") handlers.onQuery(payload as QueryEvent);
  if (name === "status") handlers.onStatus(payload as StreamStatus);
}
