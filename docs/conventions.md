# Conventions

Authoritative DNS server for Linux. Binary and CLI are named `weg`. The name is not
translated and not explained in the UI or the docs. English docs may place it with a single
aside ("German for signpost"); no wordplay is built on it.

Everything committed is in English: identifiers, comments, documentation, commit messages.

This file records what the project has settled on. The mechanics of contributing are in
[CONTRIBUTING.md](../CONTRIBUTING.md), and the reasoning behind individual decisions is in
[decisions.md](decisions.md) and [adr/](adr/).

## Product thesis

The power of BIND/PowerDNS with usability that does not require reading a manual.
Start the binary, open the web UI, have a working zone in five minutes, without ever
learning zonefile syntax.

Four differentiators. Every design decision is checked against them:

1. **Automatic reverse management.** An A/AAAA record produces the matching PTR in the
   responsible reverse zone (including IPv6 nibble zones and RFC 2317 subdelegation) and
   vice versa. Conflicts are surfaced, never silently overwritten.
2. **Time travel.** Every change is a journal event. IXFR, audit log, diff view and
   rollback to any earlier zone state all derive from that one mechanism.
3. **Cluster without an external database.** Nodes are to form a cluster themselves: no
   Postgres, no etcd, no manual primary/secondary wiring. Not built; after v0.1.
4. **An interface people enjoy using.** The GUI and the CLI are both designed.

Reference points: Technitium (GUI benchmark), PowerDNS (API benchmark), Knot DNS
(performance benchmark), BIND (the counterexample for usability).

## Fixed technology decisions

Settled, and re-opened only by a reason that has not already been weighed below.

| Concern | Choice |
| --- | --- |
| Server language | Go (current stable), DNS wire protocol via `github.com/miekg/dns` |
| Small persistence | SQLite in WAL mode, cgo-free (`modernc.org/sqlite`): the binary stays static |
| Large persistence | PostgreSQL, optional, behind the same interface |
| Cluster | Raft (`hashicorp/raft`) for the control plane |
| API | REST/JSON, OpenAPI spec is the single source of truth, clients are generated |
| GUI | SvelteKit + TypeScript + Tailwind, built statically, embedded via `embed.FS` |
| CLI / TUI | Cobra for commands, Bubble Tea + Lipgloss for interactive views |
| Delivery | One static binary, plus container image and systemd unit |
| License | AGPLv3 |

Packaging names: unit `wegweiser.service`, config dir `/etc/wegweiser/`, image `wegweiser`.

## Architecture invariants

These are not negotiable. A change that requires breaking one needs a discussion first.

1. **API-first.** GUI and CLI are clients of the same HTTP API. No feature exists in only
   one of them. No client touches the database directly.
2. **Data plane and control plane are separate.** Queries are answered from an in-memory
   radix trie. A zone change builds a new immutable snapshot that is swapped in atomically
   by pointer (RCU). A zone change must **never** block an in-flight query.
3. **Persistence is an interface.** No SQL outside `internal/store/`. `Store` defines zone,
   record and journal operations; SQLite and Postgres are implementations.
4. **Everything is a journal event.** No write bypasses the journal.
5. **Zonefiles are an import/export format, not a storage format.** An RFC 1035 parser and
   writer must exist so migrating off BIND takes minutes.
6. **Config as code.** The complete state is to export to declarative YAML and import back.
   Not built. `PUT /zones/{zoneId}/rrsets` is the seam it will use: it makes the named
   RRsets exactly what the caller sent, which is what applying a desired state needs, and
   is why that endpoint is the one with no client behind it.
7. **No root.** Port 53 via `CAP_NET_BIND_SERVICE`.
8. **The database is the source of truth; the snapshot is a derived cache.** A snapshot can
   always be rebuilt from the store. Never the other way around.
9. **A journal event is a replicated command.** Its serialized form is what Raft will carry
   later. Do not invent a second write log. See `docs/adr/0002-journal-as-command-log.md`.

## Layout

```
cmd/weg/            Binary entry point, kept thin so the CLI stays testable
internal/cli/       Cobra commands
internal/cli/output Text/JSON/YAML rendering, colour and TTY rules
internal/buildinfo/ Version stamping
internal/config/     Bootstrap settings: file, environment, flags (D11)
internal/dns/       Query path, handlers, trie, snapshots
internal/zone/      Zone model, validation, reverse automation
internal/zonefile/  RFC 1035 §5 presentation format: import and export
internal/metrics/   Prometheus collectors, fed from the query path's observer
internal/stream/    Live query stream: per-watcher filters, ring buffer, sampling
internal/store/     Store interface, sqlite/, postgres/
internal/journal/   Commit and event types: data only, no persistence
internal/apply/     The write path: commands to events, serials, rollback
internal/api/       HTTP handlers, OpenAPI, auth
internal/cluster/   Raft, membership, health          (planned, after v0.1)
internal/tui/       Bubble Tea views                  (planned, after v0.1)
web/                SvelteKit sources (build output lands in internal/api/dist)
scripts/            Development helpers, not shipped; `make demo` is the one
docs/               Architecture documents and ADRs
```

Commands live in `internal/cli`, not in `package main`, so they can be exercised by tests
without building a binary. `cmd/weg` is a shell around `cli.Main`.

`internal/journal` holds the recorded history as plain data and imports nothing but
`internal/zone`: `internal/store` persists those types, so a journal that reached for the
store would be an import cycle. Everything that *produces* history (applying a command,
allocating a serial, rolling back) is `internal/apply`, which sits above both.

The dependency rules are enforced mechanically by `depguard` in `.golangci.yml`: SQL cannot
be imported outside `internal/store`, `internal/zone` cannot reach persistence or transport,
`internal/dns` cannot reach the database, and nothing imports `internal/api`. A lint failure
there is a design problem; raise it, do not `//nolint` it.

Settled design questions and their reasoning are in [decisions.md](decisions.md). A proposal
it already rules on needs to argue with the entry rather than ignore it, and changing one
means editing that file and saying why, not quietly diverging from it in code.

## Scope fence — v0.1

**In:** authoritative UDP and TCP, EDNS0; forward and reverse zone/record management with
reverse automation; zonefile import/export; SQLite persistence with journal; REST API with
token auth; CLI core commands; GUI with zone overview, record editor and live query stream;
Prometheus metrics and `/healthz`; single node.

**Explicitly out:** DNSSEC, recursive resolver and cache, Raft cluster, outbound AXFR/IXFR,
DoT/DoH/DoQ, Postgres backend, views and split-horizon, `weg tui`, response rate limiting
and DNS cookies (`docs/adr/0006`: v0.1 bounds the amplification factor at a measured 26.5×
and does not police the rate).

Do not build any of it early. Keep the seams so it fits later without a rewrite —
especially the journal (for IXFR) and the `Store` interface (for Postgres).

## DNS correctness

Protocol behaviour is checked against the RFC, never against intuition. Name the RFC in the
comment or commit when a behavioural decision is made. The recurring ones:

- RFC 1034 §4.3.2: canonical name search order; referral and additional-section rules
- RFC 1035 §4.1.1, TC bit; §2.3.4, label and name length limits
- RFC 1982: serial number arithmetic. Serials wrap. `zone.Serial` is a struct so that
  `<` does not compile; use `Compare`, `After`, `Before`, and `Comparable` for the pair
  RFC 1982 §3.2 leaves undefined
- RFC 2181: RRset semantics, §5.2 uniform TTL within an RRset, §10.1 CNAME restrictions
- RFC 2308: negative caching, SOA in the authority section, `min(SOA.TTL, SOA.MINIMUM)`
- RFC 3596 §2.5: `ip6.arpa` nibble format
- RFC 2317: classless `in-addr.arpa` delegation
- RFC 3597: unknown RR types must round-trip unchanged
- RFC 4343: case-insensitive comparison, case-preserving storage
- RFC 4592: wildcards, closest encloser
- RFC 6891: EDNS0, OPT, BADVERS
- RFC 8020: NXDOMAIN means nothing exists below either
- RFC 8914: extended DNS errors, used to make failures explainable in the UI

Echo the query's QNAME casing in the response (0x20 encoding), do not echo stored casing.

## Quality gates

- The wire parser is fuzzed from day one. A malformed packet must never panic.
- Table-driven tests for protocol and zone logic. Cover wildcards, CNAME chains, empty
  non-terminals, case-insensitivity, 0x20, label lengths explicitly.
- CI runs the race detector, `go vet`, `golangci-lint`.
- No global variables for state. Dependencies are injected.
- Every exported symbol has a doc comment.
- Architecture decisions land as a short ADR in `docs/adr/`.

## GUI design bar

Looks like a product team built it. No Bootstrap look, no generic admin template, no sea of
widget tiles. Dark-first with an equally maintained light mode, one accent colour, WCAG AA.
A real monospace with tabular figures for anything technical: IPs, TTLs, serials. Density
over emptiness: virtualized, compact, keyboard-driven tables with instant filtering, never
20-per-page pagination. Command palette on `Ctrl+K`. The live query stream is the showcase
feature: source, name, type, rcode, latency, filterable and pausable. Git-style diff view
for zone changes with "revert to this state". Animation 150–200 ms, easing, state
transitions only, honour `prefers-reduced-motion`. Empty states and errors are designed: an
error says what broke and what to do next.

## CLI design bar

Feels like `gh`, `k9s`, `lazygit`, `btop`, not like `named-checkconf`.
Verb-shaped and predictable: `weg zone list`, `weg record add`, `weg cluster status`.
Consistent flags, `--help` with examples everywhere. Short forms (`weg z ls`, `weg r add`)
exist; long forms are what the docs use. Aligned Lipgloss tables, colour status indicators,
spinners for slow operations, and `--json` / `--yaml` on **every** command, colour off
automatically without a TTY, `NO_COLOR` honoured. That part is mandatory.
Errors carry context and a suggestion ("did you mean").
`weg tui` opens a k9s-style full-screen view (zone tree, records, query tail, cluster
status; vim keys, `/` to search) but it is after v0.1, and the fence above is what counts.
Shell completion for bash/zsh/fish, with zone names completed dynamically via the API.

