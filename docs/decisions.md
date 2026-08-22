# Design Decisions

Resolutions for the questions raised before implementation. Each entry states the decision,
the reasoning in one paragraph, and what it obliges the implementation to do.

Decisions are not permanent. Changing one means editing this file and saying why, not
quietly diverging from it in code.

---

## D1 — Module path

`github.com/wegweiserzone/wegweiser`

Binary `weg`, systemd unit `wegweiser.service`, config directory `/etc/wegweiser/`,
container image `wegweiser`.

---

## D2 — SOA serials increment by one, always

The zone serial is an internal counter. One commit advances it by exactly one, using
RFC 1982 arithmetic. There is no unix-time policy and no `YYYYMMDDnn` date counter.

The date counter is the BIND convention and it is genuinely worse: it caps a zone at 99
changes per day, which any bulk import exceeds, and it breaks the one-commit-one-step
invariant that lets IXFR replay the journal directly instead of reconstructing differences.

**Obligation: imports seed, they do not reset.** When a zone is imported from a zonefile or
an AXFR, the incoming SOA serial becomes the starting value and counting continues from
there. Starting a migrated zone at 1 would make every existing secondary consider our data
older than what it already has (RFC 1982 §3.2) and refuse to transfer it. This is the single
most common way to break a migration, so it gets a test.

**Obligation: serial comparison never uses `<`.** A `serial.Compare(a, b)` helper in
`internal/journal` is the only permitted comparison, and using `<` on a serial is a lint
target.

---

## D3 — PTR conflicts default to first-wins, and are never silent

When a new A/AAAA record points at an address that already has a managed PTR, the existing
PTR stays. A conflict record is produced, returned by the API, and surfaced in the GUI with a
one-click "make this the canonical name" action.

Configurable globally and per zone: `first-wins` (default), `last-wins`, `multi`, `reject`.

Several names pointing at one address is the normal case: virtual hosts, a load balancer, a
service alias. `multi` would be the most literal reading of "generate the PTR", but it turns
a routine operation into a five-entry PTR RRset, which breaks the near-universal expectation
that a reverse lookup yields *the* canonical name and upsets reverse-lookup-based mail
checks, logging and access control. `reject` would fail a write for a reason that is not the
user's problem. `first-wins` is the only option that never changes an answer the user did not
ask to change.

**Obligation.** A conflict is a first-class object rather than a log line: it is returned in
the API response, listed under the zone, and clearable. A conflict that is only visible in
the server log is the same as no conflict detection at all.

**Status: returned, not yet listed.** Every write that hits one reports it, in the API
response and in both clients, and it carries the policy that decided it. Listing and
clearing are not built, because a conflict is computed during a write and nothing stores it:
that needs a table, a migration and a rule for when a conflict stops existing. Until then an
operator sees a conflict when they cause one and not afterwards, which is the weaker half of
what this entry asks for.

---

## D4 — Overriding a generated record detaches it

Editing a generated PTR directly is refused, with an error naming the source record. A user
who wants a different PTR calls an explicit **detach** operation: the record loses
`managed_by`/`managed_kind` and becomes an ordinary authored record that the automation no
longer touches.

A "pin" flag, keeping the link but overriding the value, was rejected because it creates a
third record state that every consumer has to understand, in exchange for a warning nobody
reads.

**Consequence, accepted knowingly:** a detached PTR survives the deletion of the A record it
was originally derived from. It is an authored record at that point, and deleting authored
data as a side effect of an unrelated change is worse than leaving a stale record that the
consistency check will flag.

---

## D5 — Named tokens with scopes; the GUI uses a session cookie

**Tokens.** Named API tokens with `read`, `write` and `admin` scopes. One bootstrap `admin`
token is generated on first start and printed to stdout exactly once. Tokens are 256 bits
from `crypto/rand`, shown in full only at creation, stored as SHA-256 (see
the data model document §4.7).

No users, no passwords, no OIDC in v0.1. Multi-user authentication is a distinct piece of
work (password storage, reset flows, group mapping) and does not belong in the same release
as the DNS core. The schema does not preclude it.

**Browser sessions.** The GUI does not hold a long-lived token. It posts the token once to
`POST /api/v1/auth/session` and receives an `httpOnly`, `Secure`, `SameSite=Strict` session
cookie. `localStorage` is readable by any script that gets injected; an `httpOnly` cookie is
not, which is the whole point.

**CSRF.** `SameSite=Strict` blocks cross-site form posts, but it is one layer. State-changing
requests authenticated by cookie must also carry an `X-Wegweiser-CSRF` header matching a
non-`httpOnly` companion cookie (double-submit). Requests authenticated by bearer token skip
the check: CSRF requires ambient credentials, and a bearer token is not ambient.

**Transport.** A session is refused over plain HTTP from anywhere but the machine the browser
runs on. The two silent alternatives are both worse: a cookie without `Secure` is one the
network can read, and a cookie with `Secure` is one the browser never sends back, which looks
like a login that does nothing and cannot be debugged from the page. Loopback stays allowed,
because nothing leaves the host and browsers treat the origin as trustworthy for that reason.
A bearer token works over any transport and is what a program should use.

**Where sessions live.** In memory, for v0.1. A restart therefore ends every session and the
interface asks for the token again. A session is not zone data; it does not belong in the
journal, and persisting it would cost a table, four store methods and a migration for state
that is worth nothing after twelve hours. The seam is `sessionStore` in `internal/api`; a
store-backed one replaces it without the rest of the package noticing. Revisit when the
cluster arrives, because a session pinned to one node is a session lost at every failover.

**Obligation.** Token comparison is constant-time. Unknown, revoked and expired tokens return
the same error, so an attacker cannot distinguish them. Failed authentication is rate-limited
per source address. The CSRF header is compared against the value the server holds for the
session, not against the cookie the request carried: double-submit compares two things the
client sent because a stateless server has nothing better, and this one does.

---

## D5a — One delegation rule, and it is the strict one

At a delegation point only NS may live, and below it only A and AAAA glue (RFC 1034 §4.2.1).
Everything else there is referred to the child and never answered, so storing it means
holding data the server will not serve.

The rule lives in one function, `zone.ValidateUnderDelegation`, called both by the whole-zone
check and by the incremental write path. Before, only the first had it: creating a zone with
a record below a delegation was refused, and adding the same record a moment later was
accepted. The same end state was therefore accepted or refused depending on the order it was
reached in.

Strict rather than permissive, though BIND merely warns. Strictness is reversible and a store
full of records nobody can see is not, and this server's whole premise is that it does not
let a person build something that quietly does not work.

**Consequence, accepted knowingly:** the zonefile importer will meet real zones that carry
occluded records, and cannot simply refuse the file. It gets to decide (skip them and report
what it skipped, most likely) and that is a decision for the commit that builds it, made
deliberately rather than inherited from a gap here.

---

## D6 — Reverse zones are offered, never auto-created

Adding an address with no covering reverse zone does not silently skip PTR creation and does
not create the zone. The API returns a structured hint naming the zone that would be needed;
the GUI renders it as a one-click action, the CLI as a suggestion line.

Creating a zone is an assertion of authority over a namespace. Doing it as a side effect of
adding a record would be a surprise, and for public address space it would be wrong.

**Obligation.** The hint travels in the response body as structured data, not as prose in an
error string. Both clients render it; neither parses English.

---

## D7 — RFC 2317 CNAMEs are generated when we hold the parent

When Wegweiser is authoritative for both a `/24` reverse zone and a classless child zone
under it, it generates the parent-side CNAMEs
(`10.2.0.192.in-addr.arpa. CNAME 10.0/25.2.0.192.in-addr.arpa.`) as managed records with
`managed_kind = 'rfc2317-cname'`.

This is the half of RFC 2317 that is tedious and error-prone by hand, and it is exactly the
kind of thing differentiator 1 promises to take care of. Only when we hold the parent —
records in someone else's zone are not ours to write.

---

## D8 — Journal retention is unlimited by default

Full history is kept unless an operator opts into a retention policy, configured per zone as
"keep N commits" or "keep D days". Truncation always writes a checkpoint first, so rollback
to a truncated serial still works by restoring the checkpoint and replaying forward.

Time travel is a headline feature; a default that quietly discards history would undermine
it. Operators who cannot afford the growth can bound it explicitly, and the checkpoint
mechanism means bounding it does not cost them the ability to roll back.

**Status: only the default is built.** v0.1 keeps everything, because keeping everything is
what happens when nothing truncates. The opt-in policy, the checkpoints and the metric below
are not written, and the scope fence does not list them. They are the work this entry
describes, not a description of the code.

**Obligation, once it is built.** `weg zone history` and the GUI show when history is
truncated, so a missing older state reads as the policy it is. Journal size per zone is a
Prometheus metric.

---

## D9 — The query stream filters server-side, then samples, and says so

1. Filters are applied **server-side**, before the ring buffer, so a narrow filter stays
   complete even at high query rates.
2. Sampling kicks in only when the *filtered* rate exceeds the configured cap.
3. The active sampling ratio is always displayed in the UI.

The ring buffer drops events when full rather than applying back-pressure. **Confirmed
trade:** under extreme load the showcase feature loses events rather than slowing DNS down.
The data plane never waits on observability.

A stream that drops events while looking complete misleads whoever is reading it, so the
ratio is shown in the interface rather than buried in a debug field.

---

## D10 — Zone data flows through Raft; quorum loss makes the control plane read-only

Confirmed as specified in `docs/adr/0002-journal-as-command-log.md`. There is one log.

**Accepted consequence:** losing Raft quorum blocks zone edits cluster-wide. It does **not**
affect DNS service: every node keeps answering from its local snapshot, which is the plane
that matters. A DNS cluster that stops answering queries because a control-plane majority is
unreachable would be unacceptable; one that stops accepting edits for a few minutes is
merely inconvenient.

The alternative (per-zone leadership or eventual consistency between nodes) is more
available but gives up the single ordered history that time travel and IXFR rest on. Not
worth it.

---

## D11 — The config file holds bootstrap settings only

The file contains listen addresses, store DSN, TLS material and log level. Everything else
(zones, records, reverse policy, tokens, retention) lives in the database and is therefore
reachable through the API.

Precedence: flags → environment → file.

Invariant 1 says no feature exists in only one client. A setting that lives only in a file is
a feature that exists only for whoever can SSH to the box.

**Accepted consequence:** there is no way to configure a zone without a running server. Some
operators dislike that. The answer for them is `weg apply -f zones.yaml` against the YAML
export, which is a better GitOps story than a hand-edited config file, because it round-trips
and it validates.

---

## D12 — v0.1 performance targets

| Metric | Target |
| --- | --- |
| Records, all zones | 1,000,000 |
| Records, largest single zone | 250,000 |
| Query throughput | 50,000 qps on commodity hardware |
| Query latency, in-memory hit | p99 < 1 ms |
| Commit to visible in the data plane | < 200 ms |
| Cold start to serving, 1M records | < 30 s |
| Allocations per query, steady state | 0 beyond the response buffer |

The full-zone snapshot rebuild announced in the architecture document §3.4 is acceptable only
while it meets the commit-to-visible target at 250k records in a zone. Missing it makes the
incremental copy-on-write rebuild required rather than optional.

**Status: of the seven rows, one is missed, one is part-way proven and five are estimates.**
They were written as release gates before anything existed to measure them against. What
`go test -bench` shows today, on a Ryzen 7 7700 over loopback:

| Row | Where it stands |
| --- | --- |
| Query latency | `Snapshot.Resolve` takes 27–36 ns and a full UDP exchange 1853 ns in parallel. Far inside a millisecond, but these are means: no p99 under load has been taken. |
| Allocations per query | **Missed.** An exchange costs four, not zero. `Snapshot.Resolve` itself allocates nothing; all four are in the message layer, and the TODO in `internal/dns/wire.go` names them and the parser that would remove three. |
| Query throughput | Unmeasured. The parallel benchmark implies room far past 50,000 qps, but it is an in-process loop over loopback rather than a load test, and loopback has no NIC to run out of. |
| Records at 1M / 250k | Unmeasured. Nothing here has been run against a database that large. |
| Commit to visible | Unmeasured at 250k records in a zone, which is the size the rebuild question turns on. |
| Cold start | Unmeasured. |

Measuring the rest needs a seeded database of a million records and a generator that keeps
many queries in flight, neither of which exists. Until it does, most of this table is an
estimate and should not be quoted as anything else.

---

## D13 — DCO, not a CLA

Contributions are certified with a `Signed-off-by` line (Developer Certificate of Origin
1.1). No copyright assignment, no CLA.

A CLA would keep commercial dual-licensing open, which is not a goal here, at the cost of
friction that measurably reduces drive-by contributions. The DCO is what the Linux kernel,
containerd and most current infrastructure projects use.

**License reach, acknowledged deliberately:** AGPLv3 deters some corporate adoption. PowerDNS
is GPLv2, Knot is GPLv3, CoreDNS is Apache-2.0, so Wegweiser is the most restrictive of the
field. That is the intended trade (a network-facing server is exactly where the AGPL's
service clause has teeth) and it is recorded here so it is a choice rather than a surprise.

---

## D14 — QTYPE=ANY is answered with one RRset

A query for ANY is answered with a single RRset from the name: the CNAME if there is one,
otherwise the lowest type number present. Not everything the name holds, and not the
synthetic HINFO of RFC 8482 §4.2.

RFC 8482 §4.1 permits this. The full contents of a name is an amplification lever, one small
question for one large answer, and an authoritative server cannot tell a reflection attack
from a curious operator. The other bounds in the query path exist for the same reason. The
synthetic HINFO was rejected separately, because it puts a record on the wire that is not in
the zone.

The cost is real: `dig ANY` is a common way to inspect a name and it stops working here.
The record editor, `weg record list` and the live query stream cover that, and none of them
is reachable by an attacker.

**Obligation.** The choice of RRset is deterministic, so an answer never depends on the
order records happened to be stored in. Two zones with the same records in a different order
answer ANY identically.

---

## D15 — The metrics use the Prometheus client library, and cost 5 MB for it

`github.com/prometheus/client_golang`, not a hand-written exposition format. It brings the
protobuf runtime, `procfs` and the rest of its tree with it: the binary went from 15.1 MB to
20.2 MB, a third larger.

The alternative was writing the text format ourselves. It is not hard (`# HELP`, `# TYPE`,
label escaping, cumulative histogram buckets) but it is about four hundred lines once the
Go runtime and process collectors are included, and those are the ones an operator reaches
for first when answers get slow. Four hundred lines of infrastructure we would own and have
to keep correct, to save five megabytes of a binary that is downloaded once, is the wrong
trade. Nothing about it is on the hot path: the whole cost per query is 87 ns and no
allocations.

**The endpoint needs a credential.** `GET /api/v1/metrics` is authenticated like everything
else; `/healthz` remains the only endpoint that is not, because a load balancer has nowhere
to put a token and a scraper does: Prometheus has had `authorization` in its scrape
configuration for years. What a server is asked, how often, from where and how many zones it
holds is operational detail, and a metrics endpoint is the most detailed description of a
deployment that exists.

**Obligation.** The client library has exactly one importer, enforced by `depguard`.
Everything is fed through `dns.Config.Observe`; a second importer would be a counter
registered on a registry nobody serves. Label values that come from the wire are folded to a
bounded set, so what a client asks for cannot decide how many time series exist.

---

## D16 — The web interface lives in this repository, embedded, and can be switched off

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

---

## Still to be validated, not decided

- **D12's numbers need a benchmark to be real.** They are informed estimates until the
  harness exists, and building it is part of the first performance-relevant milestone.
- **Wire-format response caching** for hot RRsets is an obvious optimisation and is
  deliberately not designed yet. It only earns its complexity if the allocation target in
  D12 turns out unreachable without it.
- **Forwarding a query this server is not authoritative for**, BIND's `forwarders`, is
  raised and not settled. The scope fence puts recursion out of v0.1, but the reasons are
  bigger than the fence. An authoritative server that forwards is an open resolver: it
  listens on port 53 on every address, and the amplification factor ADR 0006 measured at
  26.5× holds only because we choose what is in our zones. It would also put two trust
  levels on one socket (an authoritative answer carries AA and is ours, a forwarded one is
  somebody else's and unvalidated (RFC 2181 §5.4.1)) and it would put another server's
  latency inside a query path that architecture invariant 2 exists to keep from waiting. A
  forwarder is only useful with a cache, so the real cost is a cache with RFC 2308 negative
  caching, source-port randomisation, 0x20 on outbound queries, EDNS negotiation with
  upstreams and cache-poisoning defences. PowerDNS, Knot and NSD all ship this as a separate
  daemon; only BIND merged them.

  Against that: Technitium does both and is a reference point in conventions.md, and
  "one box for a small network" is a real thing to want. The options are a resolver in
  front with a stub zone (nothing to build, documented), a forwarder on a listener of its
  own after v0.1 (needs its own ADR), or a deliberate change to the product thesis.
  **Decide before writing code, not while writing it.**
