# D41 — Applying a batch rebuilds the snapshot; telling the secondaries is the leader's

- Decided: 2026-09-10

## Context

Two things happen after a change is written. The query path is rebuilt for the zones it
touched and swapped in, and the secondaries are told there is a new version. Both live in
`internal/api` today, in `republish` and `tellSecondaries`, and both run because an HTTP
handler called the applier and got a result back.

A follower has no handler. It is handed a committed entry by Raft, applies it, and no
request was ever made of it. So the after-effects have to be sorted into the ones that
belong to applying and the ones that belong to the node that accepted the change, and the
sort is not obvious from where the code sits now.

## Decision

**Rebuilding the query snapshot belongs to applying a batch.** Whichever node applies, and
whatever caused it to, the zones the batch touched are rebuilt and swapped in. A follower
that applied a change and went on answering from the state before it would be the one
failure a cluster exists to prevent, and it would be invisible: the database would be right,
the log would be current, and the answers would be wrong.

**Telling the secondaries belongs to the node that accepted the write**, which is the leader,
[D40](d40-a-write-reaches-the-leader.md) having forwarded it there. Not to applying: three
nodes applying one change would send three notifications, and a secondary would transfer the
same zone three times over.

The order RFC 1996 §4.2 asks for survives that split. A proposal returns to the leader's
handler once the leader's own state machine has applied it, so the snapshot announced is
already the one being served when the notification goes out.

**The rule behind the split, for the next thing that needs sorting:** work that keeps this
node able to answer is every node's, and work that publishes a fact about the cluster to
somebody outside it is the leader's alone. Probing a secondary is already on the second list
by [D36](d36-probing-a-secondary.md), and so is rotating the cookie secret, which is one
fact clients hold and must not be answered three ways.

## Consequences

`internal/api` keeps the notification and gives up the republish. The applier gains a way to
say what it just applied, which `Result` already carries as the commits, and the wiring in
`weg serve` is where a snapshot swap is subscribed to. Restoring a log snapshot
([D30](d30-what-a-log-snapshot-contains.md)) replaces the store wholesale, so what follows it
is a rebuild of everything rather than of the zones one batch named.

A node that applies an entry and then fails to rebuild reports it exactly as it does now,
loudly and with the instruction to restart. It is current in the log and behind on the wire,
which is a worse place to be than [D29](d29-a-node-that-cannot-apply.md)'s node and deserves
no quieter a report.

Single-node behaviour does not change. The same two things happen in the same order for the
same reasons; only the question of which component owns them is answered differently.
