# Contributing to Wegweiser

Thanks for considering it. This is a one-person side project, so a pull request may sit for
a week or two before I get to it. That is not disinterest.

This document covers the mechanics. The design conventions live in
[docs/conventions.md](docs/conventions.md), and the reasoning behind them in [docs/](docs/).

## Before a large change

Open an issue first. The architecture has a small number of deliberate
[invariants](docs/conventions.md#architecture-invariants), and a patch that crosses one of
them is one I will ask you to rework, however good the code is. A short discussion
beforehand saves us both the trouble.

Small fixes, tests and documentation improvements need no preamble.

## Developer Certificate of Origin

Every commit carries a sign-off line:

```
Signed-off-by: Jane Doe <jane@example.com>
```

`git commit -s` adds it. The line means you agree to the
[Developer Certificate of Origin 1.1](https://developercertificate.org/): you wrote the
contribution, or you have the right to submit it under the project's licence.

There is no CLA and no copyright assignment. You keep your copyright, and your contribution
is licensed under AGPL-3.0 like the rest of the project.

## On AI assistance

Parts of this repository were written with an AI assistant. The
[README](README.md#how-this-is-written) says which parts and why. Use one on your own patch
if you like; what I review is the code, not where it came from. If it wrote a substantial
part, saying so in the pull request is welcome and changes nothing about how the patch is
read.

The sign-off is unaffected either way. The DCO asks whether you have the right to submit the
contribution under this licence, which is a question about you rather than about which tools
were open while you worked.

## Commits

[Conventional Commits](https://www.conventionalcommits.org/). One commit does one thing.

```
feat(zone): generate PTR records for AAAA in ip6.arpa nibble zones
fix(dns): return NODATA instead of NXDOMAIN for empty non-terminals
docs(adr): record the decision to materialize managed records
test(zone): cover wildcard synthesis below a delegation
```

The scope is the package the change lives in, minus the `internal/` prefix: `dns`, `zone`,
`store`, `api`, and the rest of the tree that [docs/conventions.md](docs/conventions.md)
lists. What is not a Go package goes under `web`, `docs`, `ci` or `packaging`.

If a commit changes DNS protocol behaviour, name the RFC and section in the body. "Because
BIND does it that way" is not a reason. "RFC 4592 §2.2.2" is.

## Before opening a pull request

```console
$ make check
```

That is the gate. CI runs the same checks, split across parallel jobs so they finish
sooner, and adds two the gate leaves out: a short fuzz campaign and a vulnerability scan,
`make fuzz` and `make vuln`. What the gate covers in detail is what `make help` prints,
rather than a list here that would drift the first time the build changes.

For a change that leaves `web/` alone, `make test` and `make lint` are the two worth running
on their own while you work. `make demo` brings up a server with zones in it, which is the
shortest way to watch a change behave.

Building is in the [README](README.md#building). `make tools` installs the Go tools; the web
half needs Node and a browser for its tests, and `make web-deps` fetches both. The Node
version CI uses is pinned in [.github/workflows/ci.yml](.github/workflows/ci.yml).

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

The architecture is enforced mechanically too. `depguard` in `.golangci.yml` keeps SQL out
of every package but `internal/store`, keeps the query path away from the database, and
carries the rest of the dependency rules from [docs/conventions.md](docs/conventions.md). A
failure there is a design problem rather than a formatting one, so do not silence it with
`//nolint`. Raise it in the issue instead.

## Decision records

A change to how the system is put together gets a short record in
[docs/decisions/](docs/decisions/), numbered sequentially and shaped like the ones already
there. Keep it under a page. The point is to record why, so the next person does not
re-litigate it from scratch, and the index in that directory says how a record is meant to
age.

## Reporting bugs

Include the output of `weg version --output json`, what you expected, and what happened. For
DNS behaviour, a `dig` command that reproduces it is worth more than a description.

Security issues go to [SECURITY.md](SECURITY.md), not to the issue tracker.
