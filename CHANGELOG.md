# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Until v1.0.0 the
public API is unstable and may change without a deprecation period.

## [Unreleased]

### Added

#### Zone transfer

- Outbound AXFR, served from the snapshot rather than the store: no lock, no journal, and a
  transfer that started at one serial sends exactly that zone however much is committed
  while it runs. It opens and closes with the SOA (RFC 5936 §2.2) and packs each message to
  what the message allows.
- IXFR, walked from the journal between the serial a client names and the one this server
  holds. D2's one step per commit makes that chain either contiguous or absent, and the
  moment it is absent the answer is a full transfer instead (RFC 1995 §2). A client already
  at or past the current serial is answered with a single SOA.
- Who may ask is a list that starts empty and has no wildcard: addresses and CIDR prefixes,
  and after TSIG below, key names. A server nobody has configured for transfers refuses
  every one. A disabled zone is not in the snapshot and is refused like any other name this
  server does not hold.
- A transfer leaves the connection loop that writes one response per query read, because it
  is one question and many answers.
- How many transfers may run at once is bounded on its own, at eight by default and settable
  as `maxTransfers`. A transfer is bounded by the size of a zone rather than of a question,
  so without a bound of their own a handful of slow clients take every connection and the
  server stops answering queries. One arriving when they are all in use is told to come back
  rather than refused, which is what a secondary retries after.

#### NOTIFY

- Who is told is a list of addresses, empty by default, held beside the transfer list. It is
  not derived from the NS RRset, because turning a name into an address is resolution and
  D17 rules that out, and not from the transfer list either, because nothing can be sent to
  a prefix.
- A notification goes out once the snapshot is published rather than when the commit lands
  (RFC 1996 §4.2), and carries the new start of authority as the hint §3.7 provides for.
  UDP, sixty seconds apart, five retransmissions, as the note under §3.6 recommends.
- One outstanding notification per zone. A publish arriving while an earlier one is still
  being retried replaces it, so a zonefile import of hundreds of commits produces one
  notification rather than hundreds.
- A notification nobody answered is not a failed write. The commit stands, the zone is being
  answered from, and the secondary falls back to the refresh timer it used before any of
  this existed.
- An inbound NOTIFY is still answered NOTIMP. Nothing transfers into this server, so
  receiving one would be a claim it cannot honour.

#### TSIG

- A key is a name, an algorithm and a secret, in a table of its own (migration
  `0002_tsig_keys`). hmac-sha256 by default, with hmac-sha384 and hmac-sha512 offered.
  hmac-sha1 and HMAC-MD5 are deliberately absent, which D28 argues against the
  MUST-implement of RFC 8945 §6.
- The secret is stored so it can be read back, because verifying a MAC needs the material
  and not a hash of it. It is the one thing in this database that survives being read, it
  never appears in a listing, and reading it is a request of its own.
- A key in the transfer list is matched by a request carrying a signature that verifies,
  from any address at all. Every message of a transfer is signed rather than spending the
  ninety-nine unsigned envelopes RFC 8945 §5.3.1 permits.
- A signed query is answered signed (RFC 8945 §5.3). BIND signs the SOA refresh it sends
  before asking for a zone and discards an unsigned answer to it, so without this a
  secondary configured with a key never reached the transfer at all.
- A refusal says which of the three failures it is. BADKEY and BADSIG go back unsigned;
  BADTIME goes back signed and carries this server's clock, because a clock difference is
  the one a client can diagnose from the answer it gets.
- Revoking a key clears its secret and frees the name, so rotating a key a secondary already
  has configured does not mean renaming it there.
- Keys are published to the query path the way the transfer list is, so a new one works
  without a restart and an unsigned query still costs a nil check.
- A notification to a secondary that names a key is signed on the way out.

#### API

- `/tsig-keys` and `/tsig-keys/{keyId}` create, list and withdraw a key, and
  `/tsig-keys/{keyId}/secret` reads one back.
- Settings gained `transferAllow` and `notifyTargets`. By D11 that is where they belong: in
  the database and reachable from every client, rather than in a file only whoever can log
  in to the machine can edit.
- `GET /zones/{zoneId}/check` says what is wrong with a zone as it stands, as a list rather
  than as a refusal. It applies the rules the write path enforces to what is already stored,
  which is how it reaches data the write path never saw: written before a rule existed, or
  put there by a hand on the database file. Each finding says whether the write path would
  refuse it or merely thinks it is a mistake, because a zone missing a glue record is not in
  the same condition as one holding a record nothing can answer. A name server this zone
  points at and has no address for is the first of the second kind: correct DNS, and a
  resolver sent to it is told the name does not exist (RFC 1912 §2.8).
- `GET /zones/{zoneId}` carries that same diagnosis, sentence included, so a client shows it
  without deriving it. Both clients used to work it out themselves, from one request per
  name server, and the two implementations were kept in step by hand.
- `GET /zones/{zoneId}/check?reverse=true` adds what reverse automation would generate for a
  zone and has not, and the addresses two names are claiming at once. Separate from the rest
  of the check because working it out plans the write that would fix it, which holds the
  zone while it runs. A conflict is worked out from the records every time rather than
  stored, so it stops being reported the moment it stops being true.
- `POST /zones/{zoneId}/reconcile` writes those entries. A reverse zone created for a network
  already in use has no change to react to and so starts empty, however many addresses are
  already named in it; this is what fills it, in one commit. It only adds.

#### CLI

- `weg tsig create|list|show|revoke`.
- `weg settings set --transfer-allow` and `--notify`, the second taking an optional
  `key:<name>` after an address.
- `weg zone reconcile` fills in the reverse entries a zone was missing, and
  `weg zone check --reverse` says what it would do without doing it.
- `weg zone check` lists what is wrong with a zone as it stands, one block per finding,
  each headed by whether the server would have refused it. It exits zero either way: the
  findings are the answer rather than a failure of the command. `--output json` carries the
  list and the counts a script would otherwise tally itself.

#### Web interface

- A Check tab on a zone, listing what is wrong with it as it stands, told apart by whether
  the server would have refused it. The reverse entries a zone is missing are asked for
  rather than assumed, and can be written from the same screen.
- A screen for the transfer keys, beside the settings that name them.

#### Observation

- `weg_dns_notifications_total`, labelled by outcome: one per datagram written,
  retransmissions included, and one per secondary answered or abandoned.

### Changed

- The write path is split in two. `Plan` resolves a command against the zone as it stands,
  mints every identifier and timestamp, and writes nothing that survives; `ApplyBatch`
  carries the plan out and makes no decisions. Applying one batch twice changes things once.
  Zone creation, update, deletion, rollback and reconciliation all plan first. This is the
  shape D24 asks for, and the batch is what a cluster would replicate.
- A batch that arrives from a replicated log records the position it arrived from, in the
  same transaction that carries it out (migration `0003_applied_index`). A node replays its
  log after a restart, and this is what lets it tell the entries it has already applied from
  the ones it has not, without asking the journal. Nothing writes an index yet: on a single
  node no batch travels, and the table stays empty.
- The decisions and the architecture decision records are now one numbered series in
  `docs/decisions/`. `docs/adr/` and `docs/decisions.md` are gone and links into them break:
  the twelve ADRs are D18 to D28, and ADR 0007 merged into D17, which was a summary of it.

### Fixed

- Creating a reverse zone for a network already in use left it empty, and switching reverse
  automation on for a zone that already had records generated nothing. Reverse automation
  reacts to changes, and in both cases there is no change to react to; each now writes what
  it implies, as a commit of its own. Switching automation off still takes nothing away.
- Delegating a name that already had records beneath it was accepted, and left those records
  in the zone with nothing ever answering them. A write is now checked against the names a
  new delegation puts out of reach as well as against the names it touches, so the rule holds
  whichever order the two records are written in.
- A rollback read its range of commits in the order they were recorded, and an identifier is
  random below the millisecond, so two commits inside one millisecond had no order at all.
  The range follows the serials now.

## [0.1.0] - 2026-08-22

### Added

#### DNS

- Authoritative UDP and TCP listeners. `SO_REUSEPORT` datagram sockets, one per processor,
  answered in the reading goroutine; connections read with an idle timeout and answered in
  order. Shutdown drains queries in flight. Measured against the wire library's own server
  over loopback: 1874 ns per query against 4751, four allocations against twenty-one.
- Query resolution as a pure function of a snapshot and a question: the canonical name
  search of RFC 1034 §4.3.2, referral before local data below a delegation, NODATA for an
  empty non-terminal, wildcards only where no closer name exists (RFC 4592), and the SOA at
  the negative-caching TTL of RFC 2308. `ANY` returns one RRset (RFC 8482), CNAME chains cap
  at eight, and NS, MX and SRV targets bring their addresses.
- Message layer with EDNS0 (RFC 6891), truncation, extended DNS errors (RFC 8914) and QNAME
  case echoed as asked (0x20). Malformed queries are answered in a fixed order of checks;
  only an unreadable header or a message that is already a response is dropped. Fuzzed from
  the first commit.
- Immutable snapshot published by atomic pointer swap, so a zone change never blocks a
  query. Empty non-terminals, delegations and wildcards are resolved at build time.
  `dns.Rebuild` builds one from a store, which is both startup and crash recovery.
- Connections bounded at 150, settable with `--max-tcp-clients`.

#### Zones and records

- Zone model in `internal/zone`: names, record types, classes, TTLs, canonical record data,
  RFC 1982 serial arithmetic and SOA parameters. Fuzzed from the first commit.
- Address-to-reverse-name mapping, including IPv6 nibble form (RFC 3596 §2.5) and RFC 2317
  classless delegation.
- Validation of zone and record aggregates, including RFC 2181 RRset semantics and one
  delegation rule (RFC 1034 §4.2.1) shared by the whole-zone check and the write path.
- Automatic reverse management: an address record generates the matching PTR in the
  responsible reverse zone and the entry follows the record it came from. RFC 2317
  parent-side CNAMEs are generated where this server holds both sides. Conflicts (D3) and
  missing reverse zones (D6) are returned as structured data, not logged. A generated record
  can be detached and taken over (D4). A reverse zone created for a network already in use
  is filled by reconciling it.

#### Storage and history

- ULID identifiers in `internal/id`; journal commit and event types in `internal/journal`.
- `Store` interface in `internal/store` with a SQLite implementation: embedded forward-only
  migrations, split read and write connection pools, connection settings verified at
  startup, cursor paging, longest-prefix reverse zone lookup, and `IterZones` for a snapshot
  rebuild. `internal/store/storetest` is the conformance suite every backend has to pass.
- Write path in `internal/apply`: commands become record changes, record changes become
  journal events, and the serial advances by one, in a single transaction. Zone creation,
  update and deletion take the same path, so no write bypasses the journal. A commit
  outlives the zone it describes.
- Rollback to an earlier serial, written forward as a new commit rather than rewound: a
  secondary that has seen a higher serial would refuse a jump back (RFC 1982).

#### Zonefiles

- RFC 1035 §5 reader: `$ORIGIN`, `$TTL`, parenthesised records, comments, `@`, relative
  names, omitted owners, classes and TTLs. `$INCLUDE` is refused, and the number of records
  a file may become is bounded.
- Writer: absolute names, explicit TTL and class, canonical order of RFC 4034 §6.1.
  Exporting the same zone twice produces the same bytes, and `named-checkzone` reads it.
- Whole-zone import. The incoming SOA serial is the serial the zone starts at (D2). Records
  a delegation would make unanswerable are skipped and reported rather than failing the file.

#### API

- OpenAPI specification in `internal/api/openapi.yaml` as the single source of truth, with
  models, server interface and client generated from it. CI fails if the generated code
  drifts.
- Zones and records: create, list, read, update, delete. A write is answering on the wire
  before the response reaches the client. RRset replacement sends the intended contents and
  the server computes the difference; several sets in one request become one commit.
- History: commits listed newest first, filtered by zone, kind, actor or time, each carrying
  the records it removed and added. The history of a deleted zone survives under the name it
  had. Rollback exposed as an endpoint.
- Failures are RFC 9457 problem documents.
- Bearer tokens with `read`, `write` and `admin` scopes, rate-limited per source address on
  failure. The first start mints an administrator token and prints it once. Managing tokens
  needs `admin`, and revoking the last administering token is refused.
- Browser sessions: a token exchanged once for an `httpOnly` cookie, with a CSRF header on
  state-changing requests (D5). Refused over plain HTTP from another machine.
- Zonefile import and export over HTTP, with a body limit of their own.
- Zone lookup by exact name, because `search` also matches `notexample.com`.
- `GET` and `PATCH /settings` for what the server does when nobody has said otherwise. The
  reverse conflict policy of D3 lives there: it was a constant no client could reach, and is
  now read inside the write transaction, so a change takes effect without a restart. A
  conflict also carries the policy that decided it.
- `GET /api/v1/metrics`; `/healthz` is the only endpoint needing no credential.
- `GET /api/v1/queries/stream` as Server-Sent Events, with a `status` message before
  anything happens and again whenever sampling or drops change. Exempt from the request
  timeout.

#### Observation

- Every exchange is offered to one observer hook: question, answer, size, transport, source
  and duration. 63 ns with something watching, 2 ns without, no allocations.
- Prometheus metrics in `internal/metrics`: queries by transport, type and response code, a
  latency histogram bracketing the sub-millisecond target of D12, response sizes, drops,
  truncations, snapshot contents and build info, alongside the Go runtime and process
  collectors. QTYPEs with no assigned mnemonic are counted together. 87 ns per query, no
  allocations.
- Live query stream in `internal/stream`. A watcher subscribes with a filter (name and
  below, type, response code, client network) applied before anything is buffered. Under
  burst it samples and reports the ratio rather than dropping silently (D9); a watcher that
  falls behind loses its oldest events. 2 ns per query unwatched, 10 to 43 ns per watcher.

#### CLI

- `weg serve`, `weg health`, `weg version`, `weg config show`.
- `weg zone list|show|create|delete|update|enable|disable|import|export|rollback`.
  `--email` is turned into the RNAME of RFC 1035 §3.3.13 with the local part escaped.
  `--ns-address` writes the name server's address record while the zone is created.
  `--auto-reverse on|off|server` spells out the third state.
- `weg record list|add|delete|update|enable|disable|detach`, written the way a zonefile is:
  names relative to the zone unless dotted, `@` for the apex, record data as the rest of the
  line. Deleting by name and type alone is refused where several records share them.
  A `TXT` whose quotes the shell ate is quoted again.
- `weg history list|show`, `weg token list|create|revoke`, `weg query tail`.
- `weg settings show|set`, with completion for the four policies.
- `weg status`: what the server has been asked and how it answered, by response code and
  question type, with the share inside a millisecond. It reads the same metrics the web
  overview does, which is the only place that knows what has been *asked* rather than what
  is *there*.
- `--output text|json|yaml` on every command, colour off without a TTY, `NO_COLOR` honoured.
  `weg token create` puts the secret alone on stdout.
- Shell completion for bash, zsh and fish, with zone names, owner names and record types
  fetched from the API and a two-second bound.

#### Configuration

- `/etc/wegweiser/config.yaml`, movable with `--config` or `$WEG_CONFIG`, holding bootstrap
  settings only (D11). A flag beats an environment variable, which beats the file, which
  beats the default, and every value records which of the four it came from.
- A misspelt key is refused, naming the key. Tokens may not be set in the file.
- `--log-level` and the `log.level` setting.
- A client on the server's own machine reads the API address from the file.

#### Packaging

- systemd unit and container image, both unprivileged with `CAP_NET_BIND_SERVICE` alone. The
  unit scores 1.4 on `systemd-analyze security`; `make unit-check` runs the verifier.
- Image is `scratch` with one static binary, about 21 MB, uid 65532, carrying its own
  configuration file and using `weg health` as its health check.
- `make demo` brings a server up on unprivileged ports with a temporary database and sample
  zones; `make demo-stop` removes it.

#### Web interface

- SvelteKit 5, TypeScript and Tailwind v4, built statically and embedded with `embed.FS`,
  served at the root beside the API (D16). `api.ui: false` serves the API alone and says so.
  Costs the binary 436 kB, 2.1 per cent.
- Signal design system: one accent colour enforced by clearing Tailwind's palette, three
  self-hosted faces (149 kB, latin subset), tabular figures for addresses, TTLs and serials,
  dark first with light applied before the first paint. `/design` renders it as a reference.
- Typed API client generated from the spec, with no HTTP library under it. The
  `@ts-expect-error` directives in `client.types.test.ts` are the test.
- Zone list and zone settings, with automatic reverse as a three-state control.
- Record editor grouped by owner name, showing what a write caused: the PTR written, the one
  refused for a conflict (D3), and the reverse zone that would be needed (D6). A generated
  record is not edited in place; the dialog offers to detach it (D4).
- Per-type record data fields (SRV, TXT, CAA and the rest) with the assembled line shown
  underneath. An unknown type still gets one box.
- Server-side paging with the filters as query parameters. No total row count, by design.
- A zone can be created from a network: `192.168.0.0/16`, the classless form of RFC 2317,
  and `2001:db8::/32` as nibbles. A bare address is refused with the network it likely meant.
- Live query stream with source, name, type, response code and latency, over charts of
  queries per second and latency distribution, and what the stream is leaving out on screen.
- History with a diff per commit and revert to an earlier state.
- Token management, and a command palette on `Ctrl+K` with `g`-prefixed shortcuts and `/`
  to focus the current filter.
- Overview built from the server's own metrics: answers by response code, questions by type,
  latency against the D12 targets, recent changes and a query rate.
- A name server the zone points at with no address for it is called out, in both clients
  (RFC 1912 §2.8).
- Import and export a zonefile: export hands the file over as a download, import takes one
  and reports what it did, including the records a delegation left out and the reverse zones
  it would need. Both existed in the API and the CLI and in neither place here.
- A settings screen for the reverse conflict policy, describing each choice by what the
  server does rather than by what the value is called.
- 53 browser smoke tests driving a real `weg` through Firefox. A page that logs an error
  fails the test.

### Fixed

- CI built against the module's declared minimum rather than the newest patch release, so
  `govulncheck` reported the standard library of the oldest permitted Go. Every job now asks
  for the newest 1.26.x, and `go.mod` requires 1.26.5.
- The reverse conflict message described first-wins whatever the policy was, so under
  last-wins a write reported both what it had written and that it had written nothing.
- A configured DNS port of zero could fail to start. The kernel picks the datagram port
  without regard to the stream ports, so the number handed out could already be taken. Only
  an unchosen port is retried; an explicit one that is taken still fails.
- A token's "last used" is written. `TouchToken` existed with a conformance test and nothing
  called it, so every token read "never". Writes are batched into one transaction every
  thirty seconds rather than made per request.
- `CreateZone` takes only the start-of-authority fields the client has an opinion about.
  Requiring all of them meant sending five timers as zero and being refused for a refresh
  interval nobody had mentioned.
- TCP connections are bounded. Each costs a goroutine and buffers that grow to the largest
  message the client sent, over the transport a datagram client is told to use when an
  answer does not fit.
- Faults and lifecycle events go through `log/slog` with a level and a timestamp, in logfmt
  or JSON Lines depending on `--output`. Command results still go through the printer.
- One delegation rule for both the whole-zone check and an ordinary write. The same zone was
  legal or illegal depending on the order it was built in.
- Amplification is measured and held by a test rather than assumed behind four separate
  bounds. The worst case is 26.5× (D23).
- Looking a record up by owner name is an index seek rather than a zone scan. SQLite
  preferred the sort-order index because it alone satisfied `ORDER BY sort_key`. A
  single-record commit is now 243 µs and stays there at forty thousand records, against
  1.3 ms and climbing.
- A database that cannot be opened fails immediately, naming what to check, instead of
  retrying until the busy timeout expires.
- Reverse automation is on for an applier that was never told either way. As a plain `bool`
  its zero value switched the headline feature off; it is a `*bool` now, nil meaning on.

[Unreleased]: https://github.com/wegweiserzone/wegweiser/commits/main
[0.1.0]: https://github.com/wegweiserzone/wegweiser/releases/tag/v0.1.0
