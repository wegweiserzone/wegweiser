# D48 — A witness is known by its identifier

- Decided: 2026-10-06
- Amends: [D39](d39-the-witness.md), [D44](d44-starting-and-joining.md)

## Context

[D39](d39-the-witness.md) asks four things of a cluster that has a witness among its voters.
Witnesses stay a minority of the voters. A witness that wins an election hands leadership to
a member that holds data. A write made while a witness leads is refused, saying that no
member able to take one is leading yet. And `weg cluster status` names a witness as one.
Each of them needs the cluster to know which of its voters are witnesses, and
[D44](d44-starting-and-joining.md) left how it knows to the work that builds `wegwitness`.

The knowledge cannot live in the store. A witness never decodes a batch, so it could not
read it, and it is the witness above all that has to pick a member to hand leadership to.
Raft's configuration is the other replicated thing every member holds, and it has three
fields per member: the identifier, the address, and whether it votes.

## Decision

**A witness's identifier begins with `witness-`.** `wegwitness` mints its identifier in that
form, and an identifier named in its file has to have it. A node with a store refuses to
start with one that does. The identifier is in Raft's configuration, so from the moment a
witness's addition is committed every member knows it is one, the witness itself and a
member that was off at the time included.

This fits what [D42](d42-membership-lives-in-the-log.md) already says an identifier is. It
never changes, and neither does what a member is: a witness is a different program, and a
machine that stops being a witness and starts holding data is a new member.

**The role named in a request to join and the identifier have to agree**, and a request in
which they do not is refused. A witness joins in the two steps a voter does, without a vote
first and with one once the log has reached it.

**Witnesses stay fewer than half of the voters.** The leader refuses an addition after which
they would not be, and a removal after which they would not be. One of three passes, two of
five passes, one of two does not.

**While a witness leads, a member does not forward a write to it.** It refuses the write
itself, with D39's words. **A witness that wins an election hands leadership to a voter
whose identifier does not begin with `witness-`.**

Two other places were weighed. A mark in the address would make the address something other
than where a member is reached, and every place that shows one would have to clean it first.
Asking each voter whether it is a witness leaves a witness that is off unknown, and the
minority rule would then rest on whichever voters happened to answer.

## Consequences

The prefix is a promise between two programs in two repositories. It is a constant in each,
and each points at this record rather than at the other's code.

## Where this stands

Built on both sides. A node with a store refuses a witness's identifier for itself, takes the
role in the join stream, keeps witnesses fewer than half of the voters on joining and on
removal, refuses writes while a witness leads, and names a witness in `weg cluster status`.
`wegwitness` mints its identifier in the form above and hands leadership to the first voter
without it that takes it.
