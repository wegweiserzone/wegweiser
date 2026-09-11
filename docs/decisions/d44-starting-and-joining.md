# D44 — A cluster is started through the API, and a new member asks to join from its first start

- Decided: 2026-09-11
- Amends: [D43](d43-the-cluster-transport.md)

## Context

[D42](d42-membership-lives-in-the-log.md) settled that the list of members lives in the log,
that a cluster is started once by one node, and that every other member joins by asking one
that is already in. [D32](d32-what-else-the-cluster-replicates.md) added that a node joins
with an empty store or not at all, and that only a node starting a cluster mints a bootstrap
token. Neither says through what a cluster is started, or how the asking is done.

The two answers are tied together by D32. A node that will join must not mint the token that
every fresh server mints on its first start, so it has to know it is joining before it has
done anything, which is before anybody could have asked it anything through its API.

## Decision

**A cluster is started with `weg cluster init`, through the API, on a running node.** Until
then the node is an ordinary server, bootstrap token and all, which is the position D32
gives a node that starts a cluster. Initialising refuses a node that already holds Raft
state, so it happens once. A single node that has been running for a year becomes the first
member of a cluster with everything it holds, and the interface can offer the same act,
since it is an API call like any other (architecture invariant 1).

A switch in the configuration file was rejected. A switch outlives the act it stands for. A
member repaired the way [D29](d29-a-node-that-cannot-apply.md) describes has its store and
its Raft directory discarded, and with the switch still in its file it would quietly start a
second cluster on its next start. Two clusters holding the same zones is the split that Raft
exists to rule out.

**A new member asks to join from its first start: `weg serve --join <address> --role
voter|nonvoter`.** The address is any member's cluster port. A node started this way mints
no bootstrap token, and refuses to start if its store holds replicated content, naming what
it found. A node that already holds Raft state is past joining, ignores the flag and says so,
which makes a unit file that keeps it harmless and makes restarting a repaired member with
the same line exactly the rejoin D29's repair ends with. The flag is not a list of members:
it names who to ask, once, and D42's rule that no node is a member because its file says so
stands.

**The asking travels over the cluster port, as a third stream kind.** After the proof D43
describes, the new member sends its identifier, the address it advertises and its role. A
member that leads proposes the addition and answers once it is committed. One that does not
lead answers with the leader's address, and the new member asks again there. That is the
redirect [D40](d40-a-write-reaches-the-leader.md) turned down for writes, and it is right
here for the reason D40 turned it down: the only client of this stream is a member, so
following an address is written once rather than in every client. Holding the secret is
what entitles a node to join, which is what D43 already said the secret proves.

**The role is named where the join is asked for.** D25's sentence that `weg cluster join`
takes the role lands on the flag. A witness joins by the same stream
([D39](d39-the-witness.md)), and the role field is where it says it is one. How the cluster
remembers which of its voters are witnesses, so that the minority D39 requires can be
enforced where an addition is proposed, is left to the work that builds `wegwitness`.

**Leaving is `weg cluster leave` on the member, and `weg cluster remove <id>` from any
other**, both through the API and both forwarded to the leader as D40 describes. The second
is for the member that cannot ask for itself because it is off, or stopped where D29 stops
it.

## Consequences

D43 gains a stream kind. A member built before it refuses the stream as one it does not
carry, and the new member reports that the member it asked speaks another version, which is
what it does.

The API grows the acts above and the status D29 and D42 call for. None of it is reachable
before a node has a cluster section in its file.

A member that joins and then finds it cannot apply the log stops the way D29 says, and the
same flag brings it back once it has been repaired. Nothing here rejoins by itself.
