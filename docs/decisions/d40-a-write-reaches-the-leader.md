# D40 — A write is forwarded to the leader, never bounced back to the client

- Decided: 2026-09-10

## Context

[D10](d10-quorum-loss-is-read-only.md) settled what a cluster does when there is no leader.
It says nothing about the ordinary case, which is that there is one and the request arrived
somewhere else.

Every node runs the whole API, because architecture invariant 1 has the GUI and the CLI as
clients of it and no feature living in only one of them. Nothing in the API tells a client
which node to send to, and nothing should: an operator points a name at three addresses, or
puts a load balancer in front, and a request lands wherever that sent it. Raft accepts a
proposal on the leader and nowhere else.

## Decision

**The node that receives a write forwards it to the leader and answers with what the leader
answered.** A client never learns which node is leading and never has to.

**The receiving node authenticates first, and forwards the identity rather than the
credential.** A token would survive the trip and a session cookie would not, sessions being
node-local by [D5](d05-tokens-and-sessions.md) and deliberately kept that way by
[D32](d32-what-else-the-cluster-replicates.md). One of the two working would be worse than
neither. So the node that was asked works out who is asking, and what reaches the leader is
that answer.

**The forward goes over the cluster transport**, which the shared secret authenticates
([D24](d24-what-the-cluster-replicates.md)), and never to the leader's public API listener.
An assertion about who the caller is has exactly the trustworthiness of the connection
carrying it.

**Whether a route writes is a property of the route, not of its method.** `POST
/auth/session` is a login. It creates a session, which is node-local on purpose, and
forwarding it would hand the browser a session belonging to a node it is not talking to. The
routes that write are marked as such, and the method is not consulted.

**Reads are answered where they land**, out of the local store, and a follower can be
briefly behind the leader. That is stated rather than hidden: this API is not linearizable
for reads, and making it so would cost a consensus round on every list of zones for a
staleness measured in milliseconds. Recording when a token was last used stays where the
request landed too, which is what D24 excluded it for.

Refusing with the leader's address was rejected. It makes every client implement
retry-and-follow, twice, and it puts an internal address into an error a person reads. An
HTTP redirect was rejected for the same reason plus one: a browser following it leaves the
origin its session covers.

## Consequences

The API server learns two things from the cluster, whether this node leads and where the
leader is, and nothing else about it.

Without a cluster configured, none of this exists. A single node has no leader to forward
to and no transport to forward over, and its write path is the one it has today.

While there is no leader at all, a write fails the way D10 already describes, and the error
says the cluster has no leader rather than naming this node as the problem.
