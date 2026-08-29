# D32 — What the cluster replicates besides zone data

- Decided: 2026-08-29
- Amends: [D24](d24-what-the-cluster-replicates.md)

## Context

D24 settled that the unit of replication is the resolved batch, and then described that
batch entirely in terms of zones: the deletions by record identifier, the updates and
insertions as whole records, the change to the start of authority, and the finished commits.

Three other things are written to this database, and none of them takes that path.

- **API tokens.** `internal/api/auth.go` calls `tx.CreateToken` and `tx.RevokeToken`
  straight. A token created on one node authenticates on that node and nowhere else, so
  behind anything that spreads requests across nodes the API works for some requests and
  not others, with nothing to distinguish the two.
- **TSIG keys.** `internal/api/tsig.go` calls `tx.CreateTSIGKey` the same way. A secondary
  signs its request and reaches whichever node answers; on the others the key does not
  exist and the transfer comes back BADKEY.
- **Server settings.** `internal/api/settings.go` writes the reverse conflict policy, the
  transfer list and the notify list through `PutSetting`. The policy is read while a change
  is planned, so two nodes holding different ones resolve the same command into different
  records. The batch hides that until the day the leader moves.

A cluster whose nodes disagree about who may transfer a zone, or about which of them can be
logged in to, is not one anybody can run. This is the work that has to happen before the
first node starts, not after.

Architecture invariant 4 says no write bypasses the journal, and all three already do. The
journal is zone history: a commit names a zone, the serial it came from and the one it
arrives at, and none of the three has any of those. The invariant is about zone data and
always was.

## Decision

**The batch carries them, and the definition of a batch widens to say so.** D24 called it
one command resolved against the state as it stands, ready to be carried out without
deciding anything further. A token, a key and a setting each fit that sentence exactly; only
the payload D24 happened to enumerate was narrower than the idea.

**The rule about minting is D24's, unchanged.** A token's secret and a key's secret come out
of the system's randomness, identifiers out of a generator and timestamps off a clock. All of
it is settled while the change is planned, on the node that accepted it, and travels with the
entry. Two nodes must never invent two secrets for one key.

**One kind of log entry, not two.** A second kind, with the state machine switching on a tag,
would mean two apply paths, two things to keep idempotent, and two places the applied index
is written. The path that exists already works for a batch that touches no zone: it carries
no commit, the journal has nothing to say about it, and the applied index written in the same
transaction is what makes applying it twice change things once.

**Node-local, and deliberately not replicated:**

- the applied index, which is a statement about one node rather than about the cluster;
- when a token was last used, which D24 excluded by name and for the same reason;
- browser sessions. [D5](d05-tokens-and-sessions.md) asked for this to be revisited here,
  and the answer is that they stay where they are. Through Raft a login would cost a
  consensus round and would fail while quorum was lost, which is exactly what D10 exists to
  avoid, and the cost of not replicating them is that the interface asks for a token again
  after a failover.

## Consequences

`Batch` and its wire format grow. D19's rule that evolution is additive covers it, and the
version in the codec is what an older node reads it with.

A batch that changes no zone becomes possible, so "has commits" stops being the test for
whether a batch does anything. `Batch.Empty` has to ask a wider question.

**The larger half of this work is not the wire format.** `internal/api` stops writing to the
store directly and asks the applier instead, so that creating a token, creating a key and
changing a setting are planned and applied like every other change. That is the point at
which the single write path invariant 4 describes is true of the whole store rather than of
the zones in it, and it is worth doing whether or not a cluster ever follows.

Invariant 4's wording is left for the commit that carries this out: it should say zone data,
or it describes a rule this code has never followed.

It arrives with [D29](d29-a-node-that-cannot-apply.md) and
[D30](d30-what-a-log-snapshot-contains.md), which settle the other two questions a node has
to have answered before it starts.
