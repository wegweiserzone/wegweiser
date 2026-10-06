# D47 — A member asked for the cluster's status asks every other member

- Decided: 2026-10-06
- Amends: [D42](d42-membership-lives-in-the-log.md), [D43](d43-the-cluster-transport.md)

## Context

[D42](d42-membership-lives-in-the-log.md) wants `weg cluster status` to say how far each
member has got through the log, because that column is what makes a member parked by
[D29](d29-a-node-that-cannot-apply.md) visible. `hashicorp/raft` keeps the leader's view of
each follower to itself, so no member can read that column out of Raft. What was built
instead says how far the member asked has got, and leaves the others to be asked one by one.

Every member can already answer for itself. What is missing is a way for one member to ask
the others, and [D43](d43-the-cluster-transport.md) names two things a stream on the cluster
port can carry: Raft's traffic, and a write forwarded to the leader.

## Decision

**The member asked asks every other member in its copy of the configuration, at the same
time, and puts their answers beside its own.** Each answers with what it would answer if it
were asked directly: how far it has applied the log, how far it knows it to be committed,
and whether it has left the cluster.

**A member that does not answer within a couple of seconds is shown as not reached, and
nothing is guessed about why.** D42 already settled that nothing on the network can tell a
machine that is off from one that is unreachable, and a status that guessed would be
guessing in the one place an operator goes to find out.

**The asking travels on the forward stream.** That stream already serves the API between
members: the member that forwards a write authenticated the caller and says who it was, and
the member that receives it acts with that caller's scopes. A status read is the same shape
with a read in it, made on behalf of whoever asked, with their read scope. D43's second kind
of stream is therefore the API as members speak it to each other, of which a forwarded write
is one use and this is another.

**A member asked over the cluster port answers for itself only.** The fan-out happens once,
on the member a person or a program asked, so a status costs one request to each other
member and can never go round in a circle.

A stream kind of its own was weighed. It would carry one request and one answer, and would
need its own framing and its own encoding of a status that the API already has.

## Consequences

A status is as slow as the slowest member that answers, bounded by the wait above. A
cluster in good health answers it about as fast as one member does.

How far a member has got is compared with the furthest commit any member reported, which is
the leader's when the leader answered. A member that is behind by a few entries for a moment
is a member that is keeping up; one that stays behind, or has stopped, is the reason for the
column.

A member removed while it was off is in nobody's configuration, so nobody asks it.
[D46](d46-a-member-that-has-left.md) already covers what it believes about itself.

## Where this stands

Not built.
