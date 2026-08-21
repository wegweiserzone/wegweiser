# Contributing to Wegweiser

Thanks for considering it. This is a one-person side project, so a pull request may sit for
a week or two before I get to it. That is not disinterest.

This document covers the mechanics; the design conventions live in
[docs/conventions.md](docs/conventions.md) and the reasoning behind them in [docs/](docs/).

## Before a large change

Open an issue first. The architecture has a small number of deliberate invariants (see
[docs/conventions.md](docs/conventions.md), *Architecture invariants*), and a patch that
crosses one of them is one I will ask you to rework, however good the code is. A short
discussion beforehand saves us both the trouble.

Small fixes, tests and documentation improvements need no preamble.

## Developer Certificate of Origin

Every commit is certified with a sign-off line:

```
Signed-off-by: Jane Doe <jane@example.com>
```

`git commit -s` adds it. The line means you agree to the
[Developer Certificate of Origin 1.1](https://developercertificate.org/): you wrote the
contribution, or you have the right to submit it under the project's license.

There is no CLA and no copyright assignment. You keep your copyright; your contribution is
licensed under AGPL-3.0 like the rest of the project.

## Commits

[Conventional Commits](https://www.conventionalcommits.org/). One commit does one thing.

```
feat(zone): generate PTR records for AAAA in ip6.arpa nibble zones
fix(dns): return NODATA instead of NXDOMAIN for empty non-terminals
docs(adr): record the decision to materialize managed records
test(zone): cover wildcard synthesis below a delegation
```

Scopes match the package: `dns`, `zone`, `zonefile`, `store`, `journal`, `apply`, `api`,
`metrics`, `stream`, `config`, `cli`. For what is not a Go package: `web`, `docs`, `ci`,
`packaging`.

If a commit changes DNS protocol behaviour, name the RFC and section in the body. "Because
BIND does it that way" is not a reason; "RFC 4592 §2.2.2" is.

## Before opening a pull request

```console
$ make check
```

That runs the same checks as CI: `go.mod` tidy, generated code up to date, formatting,
`go vet`, `golangci-lint`, the Go tests under the race detector, and the web interface's
type check, lint and browser tests.

The web half needs Node 22 and a Playwright browser:

```console
$ npm --prefix web ci
$ npm --prefix web exec -- playwright install --with-deps firefox
```

`make tools` installs the Go tools. For a change that does not touch `web/`, `make test`
and `make lint` are the two worth running on their own.

## Code expectations

- **Every exported symbol has a doc comment.** `revive` enforces it; write one that says why
  the thing exists rather than repeating its name.
- **Table-driven tests**, with `t.Parallel()` where the test allows it. An edge case gets its
  own entry in the table.
- **No global variables for state.** Dependencies are injected. If a test cannot construct
  the thing it tests, the design is wrong, not the test.
- **Errors are wrapped with context** and compared with `errors.Is` / `errors.As`. Sentinel
  errors live next to the interface that returns them.
- **Protocol behaviour is checked against the RFC.** Cite it in the comment.
- **Shortcuts are announced.** A `TODO` with a reason is fine; a silent placeholder is not.

`golangci-lint` also enforces the architecture mechanically via `depguard`: SQL cannot be
imported outside `internal/store`, `internal/zone` cannot import persistence or transport,
and the query path cannot reach the database. A lint failure there is a design problem rather
than a formatting one; do not add a `//nolint` to it, raise it in the issue instead.

## Architecture decision records

A change that alters how the system is put together gets a short ADR in
[docs/adr/](docs/adr/), numbered sequentially, following the shape of the existing ones:
context, decision, rationale, consequences. Keep it under a page. The point is to record why,
so the next person does not re-litigate it from scratch.

## Reporting bugs

Include the output of `weg version --output json`, what you expected, and what happened. For
DNS behaviour, a `dig` command that reproduces it is worth more than a description.

Security issues go to [SECURITY.md](SECURITY.md), not to the issue tracker.
