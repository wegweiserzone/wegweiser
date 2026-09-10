# D39 — A witness is a voter that keeps the log and applies none of it

- Decided: 2026-09-10

## Context

[D25](d25-cluster-shape.md) left an arbiter as a possibility: "a small separate program that
is a Raft voter and nothing else, in its own repository", offered as the tidier version of
its first way out of a two-node cluster. It said what such a program is for and nothing
about what it is.

The question is the one every second small deployment arrives with. Two servers is what a
small network buys, a majority of two is two, and a two-voter cluster survives nothing at
all. D25's first way out is a third `weg` with the DNS listener switched off. It works, and
it asks an operator to install a full authoritative server, a database and a web interface
on a machine whose entire job is to break a tie.

## Decision

**A witness is a Raft voter that persists the log, votes, counts towards quorum, and applies
none of it.** No store, no zone model, no query path, no API. Its state machine is handed
every committed entry and discards it.

**It is always a voter.** A non-voting witness would receive a log and do nothing whatsoever
with it, so the role is not offered.

**Witnesses stay a minority of the voters**: one of three, two of five. A majority made of
witnesses could commit an entry that no node holding data has, and since a witness hands
leadership away rather than using it, that entry would sit there until a node with a store
came back. The cluster would have its quorum and no way to spend it.

**It stands for election like any voter and hands leadership straight over.** Raft has no
voter that cannot be elected, and a witness must not lead, because a leader plans against
the state it holds and this one holds none. So it takes the win and immediately transfers
leadership to a member that has the data. That transfer is not a formality: Raft brings the
target up to date before handing over, which makes the witness the thing that repairs the
case it was elected in. A node that was down while changes were committed, and whose partner
died before it caught up, is brought current out of the witness's log and then leads. A write
attempted during that window is refused, saying that no member able to accept one is leading
yet.

**It never hands over state.** A witness snapshots an empty state machine, because that is
the only one it has, and its log compacts behind it like anybody's. A node with a store that
restored from such a snapshot would empty itself. So the snapshot says which kind of member
wrote it, a node with a store refuses to restore one, and it leaves the cluster and keeps
answering the way [D29](d29-a-node-that-cannot-apply.md) describes. To keep that from being
reached at all, a witness retains far more log than an ordinary member. It has nothing else
to spend the disk on.

**It holds the log in the clear, so it is exactly as sensitive as a DNS server.** TSIG
secrets travel in a batch, [D32](d32-what-else-the-cluster-replicates.md) having put them
there, the cookie secret travels beside them as a server setting, and the witness stores all
of it because storing the log is the whole job. Encrypting the payload under a key the
witness does not hold is possible and is not done here: it needs a second secret, a way to
distribute it and a way to rotate it, and it buys a state in which nobody can read the log
at all. This is the same trade [D24](d24-what-the-cluster-replicates.md) made between a
shared secret and mutual TLS, and it can be revisited the same way. What is written down
instead is that a witness is not a low-trust box, whatever its size suggests.

**It lives in its own repository, as `wegwitness`.** Everything in this repository is under
`internal/`, so another module cannot import the store, the zone model or the applier even
if it wanted them. A witness wants none of them. What it needs is `hashicorp/raft`, the log
store beneath it and the transport handshake, which is little enough that writing it twice
costs less than exporting half of this repository to a public API in order to share it.

## Consequences

A witness never decodes a batch, so it has no wire version to speak and a rolling upgrade
never involves it. The only thing it has to agree with the cluster about is the transport.

D29 cannot reach it either. A member that applies nothing has no entry it can fail to apply.

`weg cluster status` names a witness as one, rather than showing a member that answers no
queries and letting an operator work out whether that is the fault they came looking for.

The two-node arrangement stops being a workaround and becomes a shape the documentation can
recommend: two servers answering DNS, one small machine holding the log. D25's ladder is
unchanged above it, and a witness is available on every rung, always as a minority of the
voters.
