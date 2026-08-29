# D30 — What a log snapshot contains

- Decided: 2026-08-28

## Context

Two things here are called a snapshot. The query path answers from an immutable trie that is
rebuilt and swapped in on every change; that one is a derived cache the store can always
rebuild (architecture invariants 2 and 8). Raft's is a serialized copy of the state machine,
written so that the log in front of it can be discarded. This record is about the second,
and calls it a **log snapshot** wherever the two could be confused.

[D24](d24-what-the-cluster-replicates.md) already settled that there is compaction: Raft's
log "is compacted as soon as its entries are applied". Compaction in `hashicorp/raft` is
`FSM.Snapshot`. So the open question is not whether to write one but what goes in it, and
until that is answered the log is the whole history: the store grows without bound, a
joining node replays every entry ever written, and start-up time grows with the age of the
cluster.

A log snapshot has to bring a new node to current when it joins, bring one that has fallen
too far behind back without the entries it missed, and give a repaired node
([D29](d29-a-node-that-cannot-apply.md)) somewhere to start other than the beginning of
time.

## Decision

**A log snapshot is the replicated content of the store, written in the store's own
interchange form rather than a backend's file format.** Restoring one discards what the node
holds and rebuilds from the stream.

The alternative was to hand over the database file, which SQLite will produce consistently
and cheaply with `VACUUM INTO`. It is fast and complete, and it makes the snapshot a SQLite
artefact: a Postgres node behind the same interface could not read it, and architecture
invariant 3 spends a whole interface on not having backend knowledge outside
`internal/store`. Paying that back at the one point where nodes exchange their entire state
is the wrong trade.

**What a snapshot contains and what the cluster replicates are the same list.** D24 named one
exception and left the rest implicit; a snapshot forces the list to be written down, because
anything left out of it is data a joining node silently does not have.

Replicated, and therefore in a snapshot: zones, their records, and their journal.

Node-local, and therefore not: the applied index, which is a statement about this node; the
record of when a token was last used, which D24 excluded by name; sessions, which
[D5](d05-tokens-and-sessions.md) keeps in memory.

## Consequences

The `Store` interface grows the two operations a snapshot needs: streaming the replicated
content out, and replacing it wholesale on the way in. Neither exists today, and the second
is the first thing in this interface that discards data it was not asked about a row at a
time, so it belongs behind a name that says so.

**This record cannot close its own list.** Server settings, API tokens and TSIG keys are
written straight to the store by `internal/api`, outside any batch, and a batch carries zone
data only. So today they replicate nowhere, and a snapshot built from the list above would
leave a joining node unable to authenticate a single request. Whether they travel as further
kinds of log entry, or the batch grows to carry them, is not settled by D24 and is not
settled here either. It has to be settled before a cluster is usable, and it is recorded as
open in the index beside this file.
