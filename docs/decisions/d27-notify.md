# D27 — NOTIFY: who gets told, and when

- Decided: 2026-08-25

## Context

D26 built the transfer path, and the incremental half of it makes a change reach a
secondary in milliseconds. The news does not travel that fast. Without NOTIFY a secondary
finds out on its own refresh timer, which `zone.DefaultSOA` sets to an hour, so the fast
transfer sits idle behind a slow question.

Three things about this server shape what NOTIFY can be here.

**The default notify set is a set of names, and this server does not resolve.** RFC 1996 §2.1
defines it as every server in the zone's NS RRset except one also named in the SOA MNAME.
Those are domain names. A datagram needs an address, and turning one into the other is
resolution, which D17 settles as out for good. Where the NS names sit inside a zone this
server holds, its own records answer the question. Where they do not, nothing does, and that
is exactly the hidden primary in front of somebody else's secondaries, which is the
arrangement D25 put transfer ahead of the cluster for.

**A notification may not go out before the version it announces is being served.** RFC 1996
§4.2 sends it once the new version is established. Here the database commits first and the
snapshot is published after, so there is a window in which the journal knows a serial that no
query would be answered with. An incremental transfer that took its upper serial from the
journal would have had the same fault, and does not, for the same reason.

**Nothing in this server has ever sent a DNS message.** It listens and it answers. There is no
client socket, no retransmission, and no place to put a reply that is not an answer to a
query.

## Decision

**Nobody is told until somebody is named.** The notify set is a list of addresses, empty by
default, held beside the transfer list and reachable through the API, the CLI and the
interface like every other setting (invariant 1, D11).

**It is not derived from the NS RRset.** Deriving it would work on the installations where the
nameservers are named inside zones this server holds, and quietly do nothing on the ones where
they are not. An empty list that visibly notifies nobody is a better failure than a populated
one that silently misses half the secondaries, and the second kind is found months later by
somebody wondering why a record took an hour to appear.

**It is not derived from the transfer list either.** That list holds prefixes, and a prefix is
not a destination: nothing can be sent to a /24. The two also answer different questions. Who
may take a copy and who is worth waking are not the same set, and an operator who allowed a
network has not said which hosts in it to talk to.

**A notification carries the version it announces.** Opcode NOTIFY, AA set, one question,
QTYPE SOA, QNAME the apex (RFC 1996 §4.5 and §3.7), and the start of authority in the answer
section as the hint §3.7 provides for, so that a secondary willing to trust it can skip a
round trip. It stays a hint: §3.8 forbids a receiver acting on it alone, and this server has
no way to make it more than that until TSIG.

**It goes out after the snapshot is published, never before** (RFC 1996 §4.2). The publish is
the trigger, not the commit.

**UDP, sixty seconds, five retransmissions.** RFC 1996 §3.4 chooses UDP unless there is reason
to think otherwise, and the note under §3.6 recommends exactly that interval and that count.
Both are taken as written rather than reinvented.

**One outstanding notification per zone.** A publish that arrives while an earlier one is still
being retried replaces it instead of queueing behind it. What a secondary needs is the serial
this server is at, not every serial it passed through, and a zonefile import that produces
hundreds of commits should produce one notification.

**A notification that never arrives is not a failed write.** The commit is done and the zone is
being answered from. A secondary nobody could reach falls back to its refresh timer, which is
what it did before any of this existed. The fault goes to `OnError` and nowhere else.

**An inbound NOTIFY is still answered NOTIMP.** Nothing transfers into this server, so it is
never a secondary for anything, and implementing the receiving half would be a claim it cannot
honour. `internal/dns/wire.go` already refuses every opcode but QUERY; only the reason written
beside it changes, from a release that has shipped to a property of the design.

## Rationale

The RFC's own default was the tempting answer, and it is the one every other nameserver uses,
because every other nameserver has a resolver in reach. This one does not, by a decision that
is settled rather than pending. Building a half-working version of the default would have
meant either a cache this product does not have or a notify set whose completeness depends on
where somebody happened to put their NS records.

Making the list explicit costs one configuration step on the installation that wants
notification, and that installation has already taken the same step for the transfer list. It
is the same shape twice, which is one thing to learn rather than two.

## Consequences

Accepted knowingly: **an operator who fills in the transfer list and not this one gets working
transfers and slow news, with nothing to tell them why.** Two lists that look alike and do
different things is a real cost. The documentation has to put them side by side, and the
interface has to show them together rather than on separate screens.

`internal/dns` gains its first outbound socket and its first timer. The query path is
untouched: nothing here reads the snapshot on the way in, and the notify set is passed to it
the way the transfer policy is.

A notification is not a query and does not appear in the live query stream, which describes
what arrived. It gets a counter and the error hook.

TSIG will sign a notification as well as a transfer, which is one more reason for the notify
set to be a list of its own. A key belongs to a secondary, and a secondary is an entry here.

## Where this stands

TSIG arrived in [D28](d28-tsig.md), so a notification to a secondary that names a key is
signed on the way out. What §3.8 says about the hint is untouched: a receiver still may
not act on the serial alone.
