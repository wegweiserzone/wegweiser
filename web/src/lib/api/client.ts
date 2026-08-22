/**
 * The client every part of the interface talks to the server through.
 *
 * The types come from the OpenAPI document, which is the single source of
 * truth: `npm run generate` rewrites schema.d.ts from it, and a field renamed
 * in the spec breaks this build in the same commit rather than in production.
 * That is the whole reason the interface lives in the same repository as the
 * server (docs/decisions.md D16).
 */

import type { components, paths } from "./schema";

/** ZoneImported is what a zonefile import reports back. */
type ZoneImported = components["schemas"]["ZoneImported"];

/** basePath is where the API is mounted, carrying its version. */
export const basePath = "/api/v1";

/** Problem is a failure, as RFC 9457 describes one. */
export type Problem = components["schemas"]["Problem"];

/** Scope is what a token may do. */
export type Scope = components["schemas"]["Scope"];

/** Session is who a request is authenticated as. */
export type Session = components["schemas"]["Session"];

/**
 * ApiError is a failure the server described.
 */
export class ApiError extends Error {
  readonly status: number;
  readonly problem: Problem;

  constructor(problem: Problem) {
    super(problem.detail ?? problem.title);
    this.name = "ApiError";
    this.status = problem.status;
    this.problem = problem;
  }

  /** title is the short, stable summary of the kind of failure. */
  get title(): string {
    return this.problem.title;
  }

  /** detail is what went wrong with this particular request. */
  get detail(): string | undefined {
    return this.problem.detail;
  }

  /** conflicts are the reverse entries another name already claims (D3). */
  get conflicts(): components["schemas"]["Conflict"][] {
    return this.problem.conflicts ?? [];
  }

  /** missingZones are the reverse zones that would have to exist first (D6). */
  get missingZones(): components["schemas"]["MissingZone"][] {
    return this.problem.missingZones ?? [];
  }

  /** unauthenticated reports a session that has ended or never started. */
  get unauthenticated(): boolean {
    return this.status === 401;
  }
}

/**
 * NetworkError is a request that never reached the server.
 */
export class NetworkError extends Error {
  constructor(cause: unknown) {
    super("the server could not be reached");
    this.name = "NetworkError";
    this.cause = cause;
  }
}

/* ── Typing the operations ────────────────────────────────────────────────
 *
 * The generated `paths` describes every operation by path and method. These
 * pull the parts out of it, so that a call names a path this API has, sends
 * the body that path accepts, and is typed with what it answers.
 */

type Method = "get" | "post" | "put" | "patch" | "delete";

/** PathsFor is every path that has this method. */
type PathsFor<M extends Method> = {
  [P in keyof paths]: paths[P] extends Record<M, unknown> ? P : never;
}[keyof paths];

type Operation<P extends keyof paths, M extends Method> = paths[P] extends Record<M, infer O>
  ? O
  : never;

type QueryOf<O> = O extends { parameters: { query?: infer Q } } ? Q : never;
type PathParamsOf<O> = O extends { parameters: { path: infer P } } ? P : never;
/**
 * BodyOf is the JSON an operation accepts, or never when it accepts none.
 */
type BodyOf<O> = O extends { requestBody?: infer RB }
  ? RB extends { content: { "application/json": infer B } }
    ? B
    : never
  : never;

/**
 * ResultOf is what an operation answers with.
 */
type ResultOf<O> = O extends { responses: infer R }
  ? R extends { 200: { content: { "application/json": infer T } } }
    ? T
    : R extends { 201: { content: { "application/json": infer T } } }
      ? T
      : undefined
  : undefined;

/** Request is what a call may carry, with the parts this operation has. */
type Request<O> = ([QueryOf<O>] extends [never] ? { query?: undefined } : { query?: QueryOf<O> }) &
  ([PathParamsOf<O>] extends [never] ? { path?: undefined } : { path: PathParamsOf<O> }) &
  BodyPart<O> & { signal?: AbortSignal };

/**
 * Args decides whether the request argument may be left out at all.
 */
type Args<O> = NeedsRequest<O> extends true ? [request: Request<O>] : [request?: Request<O>];

/** NeedsRequest is true when the operation cannot be called with nothing. */
type NeedsRequest<O> = [PathParamsOf<O>] extends [never]
  ? O extends { requestBody: unknown }
    ? true
    : false
  : true;

/**
 * BodyPart makes the body required exactly when the operation requires one, so
 * that forgetting it is a compile error rather than a 400 at runtime.
 */
type BodyPart<O> = [BodyOf<O>] extends [never]
  ? { body?: undefined }
  : O extends { requestBody: unknown }
    ? { body: BodyOf<O> }
    : { body?: BodyOf<O> };

/* ── The cookies a browser session leaves ─────────────────────────────── */

/**
 * The CSRF value is repeated in a header on every state-changing request. The
 * cookie holding it is deliberately readable; it is worth nothing on its own,
 * because the server compares the header against the value it holds for the
 * session (docs/decisions.md D5).
 */
const csrfCookie = "weg_csrf";
const csrfHeader = "X-Wegweiser-CSRF";

function readCookie(name: string): string | undefined {
  for (const part of document.cookie.split("; ")) {
    const eq = part.indexOf("=");
    if (eq > 0 && part.slice(0, eq) === name) return decodeURIComponent(part.slice(eq + 1));
  }
  return undefined;
}

function changesState(method: Method): boolean {
  return method !== "get";
}

/* ── The client ───────────────────────────────────────────────────────── */

/** Options configure a [Client]. */
export interface Options {
  /**
   * onUnauthenticated is called when the server says the credential is gone.
   * The interface shows the session screen; without this every open view would
   * have to notice a 401 for itself.
   */
  onUnauthenticated?: () => void;
}

/** Client is the typed front door to the API. */
export class Client {
  #onUnauthenticated?: () => void;

  constructor(options: Options = {}) {
    this.#onUnauthenticated = options.onUnauthenticated;
  }

  get<P extends PathsFor<"get">>(path: P, ...args: Args<Operation<P, "get">>) {
    return this.#send<P, "get">("get", path, args[0]);
  }

  post<P extends PathsFor<"post">>(path: P, ...args: Args<Operation<P, "post">>) {
    return this.#send<P, "post">("post", path, args[0]);
  }

  patch<P extends PathsFor<"patch">>(path: P, ...args: Args<Operation<P, "patch">>) {
    return this.#send<P, "patch">("patch", path, args[0]);
  }

  put<P extends PathsFor<"put">>(path: P, ...args: Args<Operation<P, "put">>) {
    return this.#send<P, "put">("put", path, args[0]);
  }

  delete<P extends PathsFor<"delete">>(path: P, ...args: Args<Operation<P, "delete">>) {
    return this.#send<P, "delete">("delete", path, args[0]);
  }

  /* ── Zonefiles ───────────────────────────────────────────────────────────
   *
   * The one pair of endpoints that is not JSON: a zonefile goes in and comes
   * out as RFC 1035 presentation format. They are named methods rather than a
   * loosening of the generic calls above, because two exceptions should not
   * cost every other call its typing.
   */

  /** exportZone writes a zone out as a zonefile. */
  async exportZone(zoneId: string): Promise<string> {
    const url = new URL(fill("/zones/{zoneId}/export", { zoneId }), location.origin);
    const response = await this.#fetch("get", url, { accept: "text/dns, text/plain" });
    return response.text();
  }

  /**
   * importZone brings a zonefile in as a new zone.
   *
   * origin says which zone the file describes when the file itself does not.
   * A file carrying $ORIGIN needs none, and giving both where they disagree is
   * the server's error to report rather than this client's to guess at.
   */
  async importZone(zonefile: string, origin?: string): Promise<ZoneImported> {
    const url = new URL(fill("/zones/import", {}), location.origin);
    if (origin) url.searchParams.set("origin", origin);
    const response = await this.#fetch("post", url, {
      accept: "application/json, application/problem+json",
      contentType: "text/dns",
      body: zonefile,
    });
    return (await response.json()) as ZoneImported;
  }

  async #send<P extends keyof paths, M extends Method>(
    method: M,
    path: P,
    req?: Request<Operation<P, M>>,
  ): Promise<ResultOf<Operation<P, M>>> {
    // Inside the generic the request's shape is opaque, the intersection that
    // makes a call safe at the call site says nothing here. The typing that
    // matters has already been done by the time this runs.
    const sent = req as
      | {
          query?: Record<string, unknown>;
          path?: unknown;
          body?: unknown;
          signal?: AbortSignal;
        }
      | undefined;

    const url = new URL(fill(String(path), sent?.path), location.origin);
    for (const [key, value] of Object.entries(sent?.query ?? {})) {
      if (value !== undefined && value !== null) url.searchParams.set(key, String(value));
    }

    const response = await this.#fetch(method, url, {
      accept: "application/json, application/problem+json",
      contentType: sent?.body === undefined ? undefined : "application/json",
      body: sent?.body === undefined ? undefined : JSON.stringify(sent.body),
      signal: sent?.signal,
    });

    if (response.status === 204 || response.headers.get("Content-Length") === "0") {
      return undefined as ResultOf<Operation<P, M>>;
    }
    return (await response.json()) as ResultOf<Operation<P, M>>;
  }

  /**
   * fetch is what every request needs whatever it carries: the CSRF header, the
   * session cookie, and a refusal turned into an ApiError. The zonefile calls
   * below go through it so they are not a second, weaker path to the API.
   */
  async #fetch(
    method: Method,
    url: URL,
    opts: { accept: string; contentType?: string; body?: BodyInit; signal?: AbortSignal },
  ): Promise<Response> {
    const headers = new Headers({ Accept: opts.accept });
    if (changesState(method)) {
      const csrf = readCookie(csrfCookie);
      if (csrf) headers.set(csrfHeader, csrf);
    }
    if (opts.contentType) headers.set("Content-Type", opts.contentType);

    let response: Response;
    try {
      response = await fetch(url, {
        method: method.toUpperCase(),
        headers,
        body: opts.body,
        // The session cookie is httpOnly, so it is the browser that has to
        // attach it. Same-origin is what the API is designed for, which is why
        // there is no CORS configuration anywhere in this project.
        credentials: "same-origin",
        signal: opts.signal,
      });
    } catch (cause) {
      // An aborted request is the caller's own doing and is not a failure to
      // report as one.
      if (cause instanceof DOMException && cause.name === "AbortError") throw cause;
      throw new NetworkError(cause);
    }

    if (!response.ok) throw await this.#fail(response);
    return response;
  }

  /** fail turns a refused response into the error the interface renders. */
  async #fail(response: Response): Promise<ApiError> {
    let problem: Problem;
    try {
      problem = (await response.json()) as Problem;
      // A proxy in front of the server can answer with something that is not
      // one of ours, and a document missing the fields the interface reads
      // would render as a blank error box.
      if (typeof problem?.title !== "string") throw new Error("not a problem document");
    } catch {
      problem = {
        type: "/problems/unknown",
        title: `The server answered ${response.status}`,
        status: response.status,
        detail: "the answer was not a problem document, so there is nothing more to say about it",
      };
    }

    const err = new ApiError(problem);
    if (err.unauthenticated) this.#onUnauthenticated?.();
    return err;
  }
}

/**
 * fill puts the path parameters into the template.
 */
function fill(template: string, params: unknown): string {
  const given = (params ?? {}) as Record<string, unknown>;
  const path = template.replace(/\{([^}]+)\}/g, (_whole, key: string) => {
    const value = given[key];
    if (value === undefined) throw new Error(`api: the path needs ${key}`);
    return encodeURIComponent(String(value));
  });
  return basePath + path;
}
