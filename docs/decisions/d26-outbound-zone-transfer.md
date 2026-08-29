# D26 — Outbound zone transfer: who may ask, and what is answered

- Decided: 2026-08-25

## Context

A zone transfer is how a secondary server gets a copy of a zone, and it is the one
replication mechanism every other nameserver already speaks. D25 put it ahead of the
cluster for that reason: it makes a single Wegweiser into a redundant service using servers
that already exist, without any consensus at all.

Three things about this server shape the decision.

**An AXFR needs nothing the query path does not already hold.** The snapshot is every zone
the server answers from, and a `zoneTree` carries the apex name, the start of authority and
every node beneath it. A full transfer is a walk of one of those. Only an incremental
transfer needs the journal, and the journal lives in the store, which `internal/dns` is
forbidden to import (invariant 3, enforced by depguard).

**A transfer is one question and many answers.** `Snapshot.Resolve` answers one question with
one message, and the TCP loop in `internal/dns` writes one framed response per query read. A
transfer does not fit that shape and cannot be bolted onto it.

**The zone is the thing worth stealing.** On the networks this server is aimed at, the zone
contents are an inventory: every host, every address, every service anyone bothered to name.
An open AXFR hands that to whoever asks, and unlike a query it cannot be rate-limited into
harmlessness, because one request is the whole thing.

## Decision

**Nobody may transfer until somebody is named.** The list of who may is empty by default and
has no wildcard. A server that has never been configured for transfers refuses every one, and
that is not a mode to be switched on wholesale but a list to be filled in.

**The list names addresses and prefixes.** A transfer is a TCP session (RFC 5936 §2.2), so a
client has to complete a handshake and an off-path spoofer cannot. That makes an address a
real control here in a way it is not over UDP.

**TSIG (RFC 8945) is the next step and is not built.** It is what an address list cannot do:
tell two hosts behind one NAT apart, or authenticate a secondary run by somebody else. It
needs keys that can be created, listed and revoked, which by invariant 1 means the API, the
CLI and the interface, so it is its own piece of work rather than a flag. The list is defined
so that a key can be an entry in it later without the shape changing.

**A full transfer is served from the snapshot.** No store, no journal, no lock. It starts and
ends with the SOA (RFC 5936 §2.2) and packs as many records into each message as the message
allows. A disabled zone is not in the snapshot and is refused like any other name this server
does not hold.

**The snapshot is what makes a transfer consistent.** It is immutable and swapped by pointer,
so a transfer that started from one holds it to the end and sends exactly the zone at one
serial, whatever is committed meanwhile. That is a property this design gets for nothing and
other servers work for.

**An incremental transfer is proved, never guessed.** The server walks the journal from the
serial the client names to the one it holds, commit by commit, and D2 makes that walk exact:
one commit advances the serial by one, so a contiguous chain either exists or does not. The
moment it does not, for any reason, the answer is a full transfer instead (RFC 1995 §2). A
client whose serial equals or exceeds the server's is answered with a single SOA, as RFC 1995
§2 requires.

**Transfers are bounded separately from queries.** `maxTCPClients` bounds connections without
caring what they are for, and a large zone going to a slow client holds a slot for orders of
magnitude longer than a query does. Without a bound of its own, a handful of transfers is a
way to stop this server answering anything.

## Rationale

Defaulting to deny costs a configuration step and is the only default that can be loosened
later. The opposite mistake cannot be corrected: an installation that has been transferring
to the world for a year does not get quieter when the default changes, and the operator who
would have to notice is the one who never read the manual, which is the operator this product
is written for.

Serving the full transfer from the snapshot rather than the store was the choice that made
the first half of this small. It also keeps invariant 2 exactly as it reads: the query path
touches nothing but the snapshot, and the one path that reads the journal is a separate,
TCP-only, opt-in one that runs on its own connection.

Falling back to a full transfer rather than refusing an uncoverable incremental one is what
RFC 1995 §2 provides for, and it is the behaviour that keeps a secondary working through a
retention change (D8) instead of leaving it stuck on an old serial with an error nobody sees.

## Consequences

Accepted knowingly: **a transfer sends the zone to a client this server cannot identify
beyond its address.** On a private network behind one administration that is enough. Across
an organisational boundary it is not, and the answer there is to wait for TSIG rather than to
put an address list on the internet and call it access control. The documentation says so
where it says how to configure this.

`internal/dns` gains a path that reads outside the snapshot, through an interface it defines
and the wiring implements. It still does not import the store, and depguard still says so.

The journal becomes load-bearing for something other than history. Retention (D8) stops being
purely an audit question: pruning commits shortens how far back a secondary may ask before it
has to take the whole zone again.

NOTIFY (RFC 1996) is not part of this. Without it a secondary finds out about a change when
its refresh timer fires, which is correct and slow. It is the obvious next piece and it needs
its own decision about who gets told.

## Where this stands

Both pieces this record left for later exist. NOTIFY is [D27](d27-notify.md), and TSIG is
[D28](d28-tsig.md), which makes an entry in the transfer list a key as well as a prefix.
The decision itself is unchanged: the list starts empty and has no wildcard.

The bound this record called owed is built. `maxTransfers` limits how many transfers run at
once, inside the connections `maxTCPClients` allows, and it defaults to eight. A transfer
arriving when they are all in use is answered SERVFAIL with the extended error "Not Ready",
not REFUSED: the answer is "not now" rather than "not you", and a secondary retries on the
timer its SOA already carries. The slot is claimed after the request is authorised, so a
client that may not transfer cannot spend the budget by asking.
