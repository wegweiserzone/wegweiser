# D16 — The web interface lives in this repository, embedded, and can be switched off

One repository. `web/` holds the SvelteKit sources, the static build lands in
`internal/api/dist/` and is embedded with `embed.FS`, exactly as the technology table in
conventions.md already said. This entry exists because the alternative was raised (a second
repository, so that people who only want the command line are not handed an interface they
did not ask for) and the reasoning deserves to be written down rather than re-argued.

**What a second repository would cost.** The product thesis is "start the binary, open the
web UI, have a working zone in five minutes". Two artefacts turn that into: install the
server, install the interface, tell the interface where the API is, arrange CORS, serve the
files from somewhere. That is not five minutes, and differentiator 4 becomes an extra most
deployments never set up. CORS is the sharper edge: today the API needs none, because the
page is same-origin, and D5's `SameSite=Strict` session cookie is built on that. A separate
repository does not force a separate origin but invites one, and the answer to it is a
configurable origin allowlist where there is currently nothing to get wrong. The generated
TypeScript client is the third cost: in one repository a change to `openapi.yaml` breaks the
interface's build in the same commit and the same CI run, which is what `generate-check`
already does for the Go client. Across two, it breaks later, in somebody else's pipeline,
after the API has shipped.

**What the interface actually costs a command-line user.** Bytes and one route. Not
capability: architecture invariant 1 means no feature exists only in the GUI, so the CLI is
never the lesser client. Not attack surface either, in the sense that matters: sessions,
CSRF and the transport check of D5 already exist and are not removed by moving files
somewhere else.

**The switch is a setting, not a repository.** `api.ui: false` in the config file (a
bootstrap setting, D11) makes the server not register the interface's routes at all: `/`
answers 404 and only `/api/v1` and `/healthz` exist. That covers the operator who does not
want a web interface reachable on a nameserver, and it costs them no custom build. A
`noui` build tag that leaves the assets out of the binary entirely is *not* built now; it is
some twenty lines and can be added the day a distribution packager asks for it.

**The build output is committed.** `internal/api/dist/` is tracked, so `go build` and
`go install .../cmd/weg@latest` produce a complete binary on a machine with no Node
installed: the same reasoning that already applies to the generated API code in
`internal/api/gen/`, and the alternative is an install path that silently yields a binary
with no interface. The price is a repository that grows by a set of hashed asset files
whenever the interface changes.

**Obligation.** `.gitignore` stops ignoring `internal/api/dist/`. A `make web` target builds
the interface and a `web-check` target fails when the committed output does not match the
sources, in the same shape as `generate-check`. The embed must not break on a tree that has
never run `make web`, so a placeholder `index.html` is committed from the start. The
interface is served under `/` with an SPA fallback; the API keeps `/api/v1` and nothing about
the routing is allowed to depend on a build having run.
