# D46 — A member that has left keeps answering, and says it has left

- Decided: 2026-10-06
- Amends: [D42](d42-membership-lives-in-the-log.md), [D44](d44-starting-and-joining.md)

## Context

[D42](d42-membership-lives-in-the-log.md) made leaving an act, and
[D44](d44-starting-and-joining.md) gave it two commands: `weg cluster leave` on the member,
and `weg cluster remove <id>` from any other. Neither says what the node is once the act is
done.

`hashicorp/raft` gives three different answers, depending on where the node stood. A
follower that is removed is sent the change, stays a follower with no leader, and never
stands for election again. A leader that removes itself stops Raft altogether. A member that
was switched off when it was removed is never told, and on its next start it asks for votes
the others turn away. Left to itself, the first refuses writes because the cluster seems to
have no leader, the second fails when asked for its status, and the third believes it is
still a member.

## Decision

**A member that has left is in the position [D29](d29-a-node-that-cannot-apply.md) gives
one that could not apply the log.** It goes on answering queries with what it held, refuses
every write with an error saying that it has left the cluster, and says so where an operator
looks: in `weg cluster status`, in the `current` field of `/healthz`, and in the metric D29
added. Nothing brings it back by itself.

From there the operator chooses. Joining again is D29's repair: discard the store and the
Raft directory and start the node with `--join`. Going on alone means discarding the Raft
directory only. The node then starts as a single server holding what it held, which is the
position D44 gives a server that has never been in a cluster, and `weg cluster init` can
make it the first member of another.

Two alternatives were weighed.

**Turning into a single server at once**, by dropping the Raft state as part of leaving, is
the convenient one. It is also a server that takes writes the moment it has left, while
resolvers may still be sending it queries, and that drifts away from the cluster on the
first one without anybody having decided that it should. D44 turned down a switch in the
file for the same reason: the act that takes a node out of a cluster should not also be the
act that makes it a different server.

**Stopping the DNS listener** is what [D10](d10-quorum-loss-is-read-only.md) and D29 already
refuse. A node that has left holds data that was right a moment ago. Whether it stays in
rotation is the operator's call, as it is for one that is behind.

## Consequences

A member is known to have left when its own copy of the configuration no longer lists it,
or when Raft has stopped on it without a stall. Both survive a restart, since the
configuration is in the log the node keeps.

The member that was off when it was removed is the exception: nothing tells it. The others refuse it a vote, it cannot lead, and its writes are refused
as made while no member leads, until it is repaired. Nothing it does can split the cluster.

Raft refuses to remove the last voter. It does not refuse a removal that leaves a majority
the remaining voters cannot reach, such as taking out a running voter while another is off.
The order is the operator's: remove the member that is off first.

## Where this stands

Built as described, in `internal/cluster` and on the API's
`DELETE /cluster/members/{memberId}`. `weg cluster status` on a member that has been taken
out says so and names both ways on, `/healthz` reports it as not `current`, and
`weg_cluster_behind` is 1, the same three places a member stopped by D29 shows up. A member
removed while it was off still believes it is one, as described above.
