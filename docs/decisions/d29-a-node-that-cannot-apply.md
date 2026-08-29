# D29 — A node that cannot apply an entry leaves the cluster and keeps answering

- Decided: 2026-08-28

## Context

[D24](d24-what-the-cluster-replicates.md) settled that the state machine never rejects:
everything a change could be refused for was refused by the plan, on the node that accepted
it, before the entry was proposed. It did not say what happens when a node fails to carry an
entry out anyway.

That failure has three shapes, and they do not behave alike:

- **The entry cannot be read.** A batch carries a version, and an older build meets one it
  does not speak. That is a rolling upgrade done in the wrong order. It is deterministic:
  trying again gives the same answer.
- **The write fails.** A full disk, an I/O error, a damaged database. Some of these clear
  while the process runs and some never do.
- **The batch is well formed and the store refuses it**, which is a bug in this program.

`hashicorp/raft` has no way to say "not yet". `FSM.Apply` returns a value that reaches the
caller through the `ApplyFuture` on the leader and is dropped on a follower; Raft advances
either way and never offers the entry again. Returning an error is therefore the same act as
skipping the entry, and D24 already has the name for that: a follower that diverges without
saying so.

## Decision

**A node that cannot apply a committed entry leaves the cluster and goes on answering
queries.** It stops taking part in Raft, keeps the query snapshot it holds, refuses every
write with an error saying that this node is behind rather than that the cluster is, and
says so where an operator looks. It does not rejoin by itself.

A bounded number of retries comes first, because the full disk that somebody empties is a
real failure and giving up on the first attempt would be wrong. The count is fixed rather
than configured: an operator has no information with which to choose it.

Two alternatives were weighed.

**Stopping the process** is what etcd and Consul do, and it is loud, which is the property
that matters. It also takes the DNS listeners down, and that is the thing
[D10](d10-quorum-loss-is-read-only.md) refuses: a server that stops answering because its
control plane failed is unacceptable, while one that stops accepting edits is inconvenient.
A node that missed an entry is behind, not wrong. Every name it holds it still answers
correctly as of the last entry it applied.

**Retrying until it works** is right for the failure that clears and wrong for the one that
does not, and nothing inside the process can tell them apart. A node retrying a batch it
will never be able to read blocks every entry behind it and looks exactly like one that has
hung.

**The uncomfortable half is deliberate.** A node that has left the cluster keeps answering
data that is behind. It is stale and internally consistent, which is the position of a
secondary past its refresh and before its expiry, a state DNS has had a word for since RFC
1034. Answering what was true at a known moment beats answering nothing. The operator
decides whether to take it out of rotation, and everything above exists so that they can
know to.

## Consequences

`weg cluster status` says which nodes are current, and a node that is not names the entry it
stopped at and why. A metric carries the same fact for whoever is watching rather than
looking.

`/healthz` keeps meaning what it means. Today it answers whether there is a query snapshot
to serve from, which is a data-plane question, and a node that is behind can still answer
that with yes. Whether the node is current is a second field beside it rather than a
different status code, because changing what an existing probe returns would take nodes out
of rotation that an operator had not decided to remove.

Until [D30](d30-what-a-log-snapshot-contains.md) is built, repairing such a node means
discarding its store and its Raft log and letting it replay from the beginning. That works
only for as long as nothing compacts the log, which is exactly what D30 ends.
