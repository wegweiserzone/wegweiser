# D42 — Membership lives in the log, and a node joins by asking a member

- Decided: 2026-09-10

## Context

Three records already constrain how a node starts. [D11](d11-config-holds-bootstrap-only.md)
keeps the config file to what is needed before anything is running.
[D32](d32-what-else-the-cluster-replicates.md) settled that a node joins with an empty store
or does not join, because a node that minted its own bootstrap token on the way in would
carry an administrator credential the cluster never issued. [D25](d25-cluster-shape.md) gave
`weg cluster join` a role to take.

What none of them says: what identifies a member, where the list of members is kept, and how
one stops being a member.

## Decision

**The list of members lives in the log, and the config file does not have one.** What a node
reads from its file is what it needs in order to be reachable at all: an identifier, an
address to listen on, the address to advertise to the others, and the secret the transport
authenticates with (D24). Who else is in the cluster is Raft's configuration, which is
replicated like everything else. A second list, in a file, on each machine, edited by hand,
is how a cluster ends up with two answers to the only question that must have one.

**A cluster is started once, by one node**, which brings itself up as the single voter and
is the node D32 lets mint a bootstrap token. **Every other member joins by asking one that
is already in**, and that member proposes the addition. No node adds itself by naming the
others in its file, and no node is a member because a file says it should be.

**The identifier is minted on first start and never changes.** Raft knows a member by it,
and handing a fresh machine the identifier of an old one grafts one member's history onto
another's. It is kept beside the applied index as node-local state, and an operator may set
one explicitly in the file instead. The hostname is not it: a machine that is renamed is the
same member, and two machines renamed to match are not one.

That sharpens D32's rule rather than bending it. A node joins with a store empty **of
replicated content**. Its own identifier and its applied index are already there, having
been written before it ever asked to join, and neither is anything the cluster issued.

**Leaving is an act, not an absence.** A member is removed when somebody says so, through
`weg cluster leave` on the node or by naming it from another member. A node that is merely
switched off stays a member and keeps being counted, because nothing on the network can tell
a machine that is off from one that is unreachable, and a cluster that removed members for
being quiet would shrink its way out of a partition and into two clusters.

A witness ([D39](d39-the-witness.md)) starts, joins and leaves by the same three acts, with
the same file holding the same four things.

## Consequences

The config file grows a cluster section, and a file without one is a single node whose
behaviour is unchanged in every respect.

`weg cluster status` answers from the replicated configuration plus what this node knows
about the others: who is a member, which role each holds, which is leading, and how far each
has got through the log. That last column is what makes a node parked by
[D29](d29-a-node-that-cannot-apply.md) visible as one.

The first node's own address is in its configuration from the moment it starts a cluster, so
a member that changes address is a change to that configuration rather than a restart with a
different file.
