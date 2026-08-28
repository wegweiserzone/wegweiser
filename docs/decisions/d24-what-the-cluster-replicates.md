# D24 — What the cluster replicates

- Decided: 2026-08-22
- Amends: [D19](d19-journal-as-command-log.md)

## Context

D19 settled that there is one log and that the journal is it. It went further and
described the write path as already being a state machine: "no globals, no `time.Now()`
inside, no random IDs generated during apply. Timestamps and IDs are assigned when the
command is accepted and travel with it."

Read against the code as it stands, that is not yet true, in three ways that all matter.

**Identifiers are minted while applying.** `internal/apply/reverse.go` mints a record ID for
every generated reverse entry, `rollback.go` for every record a rollback puts back, and
`commitAs` for the commit itself. They are ULIDs, so time plus randomness. Two nodes applying
the same input would hold the same records under different identities, and a record ID is not
internal: it is in the API, in the GUI, and in the URL a client edits through.

**The clock is read while applying.** The commit takes `a.now()` inside the write
transaction, so each node would stamp its own time on the same change.

**`[]Event` cannot rebuild the store.** `eventFor` carries owner name, class, type, TTL and
rdata, and drops the record's identity, its `ManagedBy` provenance link and its comment. That
is right for what the journal is shaped for, since RFC 1995 §2 describes a difference
sequence and nothing more. It also means a node that replayed events would hold records with
no provenance, which breaks D4's detach and the reverse automation on any node that later
became leader.

D19 asked the right question, replicate facts rather than intent, and landed one step
past the answer. A row-level change is a fact too.

## Decision

**The unit of replication is the resolved batch.** One entry carries the deletions by record
identifier, the updates and insertions as whole records, the change to the start of
authority, and the finished commits, across every zone the command touched.

That is what `internal/apply/changes.go` already assembles as `changeSet`. It becomes a
public, serializable type rather than a private one.

The write path splits in two:

```go
// Plan resolves a command against the zone as it stands. It mints every
// identifier and every timestamp, and writes nothing that survives.
func (a *Applier) Plan(ctx context.Context, cmd Command) (*Batch, error)

// Apply carries out a batch. It makes no decisions.
func (a *Applier) Apply(ctx context.Context, b *Batch) (*Result, error)
```

**The state machine never rejects.** Anything a plan could have caught is caught by the plan,
on the leader, before the entry is proposed. A follower that failed to apply an entry the
cluster had already committed would diverge without saying so, and there is no error path
that is better than not needing one.

Validation reads back what was written rather than simulating it, which `validateTouched`
does on purpose, because a simulation is a second model of the same thing and the two drift.
So `Plan` performs its writes inside a transaction it then rolls back. The leader pays for
every write twice. At the rate people edit zones, that is not a cost worth designing around.

**The applied index is written in the same transaction as the batch.** Raft replays from its
last snapshot when a node restarts and knows nothing about what the store already holds. An
applied index kept anywhere other than inside the same commit is a cluster that quietly
corrupts itself after a power cut.

**Raft's own log lives in BoltDB**, through the stock `raft-boltdb`, not in SQLite.

This asks D19's sentence to be read precisely. "There is one log" means one application
history: one ordered account of what happened to the zones, which the journal is and remains.
Raft's log is transport. It runs ahead of the journal, is written before it, is never in a
position to disagree with it, and is compacted as soon as its entries are applied.
Implementing `raft.LogStore` over SQLite would make the sentence literally true and was
considered. It was rejected because a log store sits on the write path, its failures appear
under load rather than in tests, and there is one person here.

**`TouchToken` does not replicate.** It records that a token was used and runs on every
authenticated request. Through Raft it would cost a consensus round per API call and would
fail every authenticated read while quorum was lost, which is exactly the failure D10 exists
to avoid. It stays a node-local write and is a named exception to architecture invariant 4.
It is defensible because it is not zone data and does not claim to be replicated: what it
answers is when a token was last used *here*.

**The Raft transport authenticates with a shared secret** read from the configuration file,
which D11 allows because it is needed before anything is running. Mutual TLS with a cluster
CA is better and can replace it without disturbing anything above the transport. What is not
available at any stage is an unauthenticated control plane that can rewrite every zone this
server answers for.

## Consequences

The first phase contains no distributed system at all: split `Plan` from `Apply`, make
`Batch` serializable, move identifier and timestamp minting into `Plan`, add the applied
index. It stands on its own, because three of the findings above are places where the shipped
code and D19 disagree on a single node today.

`Batch` becomes a wire format, so D19's rule that evolution is additive applies to it
rather than to `Event`. `Event` is free to stay exactly what RFC 1995 needs.

Reverse automation keeps its atomicity across zones, because one batch spans every zone one
command reached. That is the only cross-zone transaction this data model has.

The doubled write on the leader is real and measurable. If it ever matters, the answer is to
make validation work against a plan, not to stop validating.
