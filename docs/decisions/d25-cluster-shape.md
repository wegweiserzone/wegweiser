# D25 — Cluster shape: a few voters, and everything else replicates

- Decided: 2026-08-22

## Context

Raft is the fixed choice for the control plane, and D10 settled what happens when quorum is
lost. Neither says how many nodes there should be, what to do below three, or what happens
well above seven. Those are the first questions an operator asks, and leaving them open means
answering them in code, one deployment at a time.

The comparison people arrive with is BIND, which does not cluster. Primary/secondary with
AXFR, IXFR and NOTIFY replicates a zone; Raft replicates a database. They differ in the unit,
in whether the primary is elected or configured, in whether a write waits for anyone, and in
whether the other end has to be the same program. The zone transfer also expresses something
a cluster has no word for: a server authoritative for one zone and secondary for another that
somebody else runs.

They are two axes, not two answers, and this project wants both. What follows is only about
the cluster axis.

## Decision

### Voters carry the consensus, replicas carry the load

A cluster has three or five voting members. Everything beyond that joins as a non-voter
(`raft.AddNonvoter`): it receives the whole log, answers DNS exactly like any other node, and
neither votes nor counts towards quorum. The leader does not wait for it before confirming a
write.

| Nodes | Shape |
| --- | --- |
| 1 | Nothing. A single node is a supported deployment, not a degraded cluster. |
| 2 | See below. There is no honest two-node cluster. |
| 3 to 7 | All voters. This is the range Raft is for. |
| up to about 30 | Three or five voters, the rest non-voters. |
| beyond that, spread across sites, or mixed with other software | A cluster as hidden primary, and zone transfer outward from it. |

Voter counts are odd because even ones buy nothing: four voters tolerate one failure, exactly
as three do, and pay more latency for it. Twenty voters would need eleven acknowledgements per
write, hold elections on every network hiccup, and give a leader nineteen followers to feed.
etcd and Consul recommend three, five or seven for the same reasons, and neither goes higher.

### Two nodes

A majority of two is two, so a two-voter cluster survives no failures at all. That is
arithmetic, not an implementation limit, and with exactly two nodes an operator picks one of
two properties and cannot have both:

- **one primary, promoted by hand.** Always safe. If it is gone, DNS keeps being answered by
  both and zone edits wait for a person.
- **automatic promotion.** A partition leaves two nodes each believing they may write, and
  afterwards there are two histories of the same zone and no rule for merging them.

**Wegweiser does not ship a two-node mode that elects.** Claiming automatic failover on two
nodes would be a lie told in the one place a DNS server must not lie.

Two ways out, and the documentation offers both rather than picking:

1. **A third member that holds data and answers nothing.** Another `weg` on something small
   with the DNS listener switched off. It is an ordinary voter, costs almost nothing, and
   turns two into three.
2. **No cluster at all.** One Wegweiser as primary and the second server as an ordinary
   secondary over AXFR/IXFR. No consensus, no election, no split brain, and the second server
   may be BIND, Knot or NSD. For a small network this is frequently the better answer, and
   saying so is more useful than selling the cluster.

A dedicated arbiter, a small separate program that is a Raft voter and nothing else, in its
own repository, would make the first option tidier than switching off half of a full server.
It is recorded here as a possibility and is not part of this decision.

### Rejected: a Raft group per zone

Sharding zones across independent groups, the way a distributed SQL database shards ranges,
buys write throughput. Write throughput is not this product's constraint: zones are edited by
people, and D12's numbers are about queries, which every node answers locally from its own
snapshot and which Raft never touches.

The cost would be a Raft group per zone with its own membership, and reverse automation
becoming a transaction across groups. That is the one place this data model has a cross-zone
transaction (D24), and it is the product's headline feature. Too much machinery for a
bottleneck that does not exist.

### Rejected: a replication tree of our own

Beyond a few dozen nodes, or across sites, a leader feeding every node directly is the wrong
shape and a tree is the right one. That tree already exists and is standardised: a hidden
primary, a tier of distribution servers, edge servers transferring from those. Building a
second one inside Raft would reimplement AXFR badly, and Raft over a wide-area link holds
elections nobody asked for.

## Consequences

The ladder above is documentation before it is code. Most of it needs nothing built: rows one
and three are the cluster as designed, and rows two and five are zone transfer, which is not
built yet either.

**Outbound AXFR and IXFR cover three of the five rows; Raft covers one.** Transfer turns a
single Wegweiser into a redundant DNS service using servers that already exist, with no
consensus involved. The cluster answers the narrower question of whether the control plane is
redundant too. That does not change D10 or the choice of Raft, but it does change what is
worth building first, and the roadmap in the conventions is ordered accordingly.

Non-voting members are a first-class deployment rather than a tuning knob, so `weg cluster
join` takes the role, `weg cluster status` shows it, and a node that is not a voter says so
plainly instead of looking like a broken one.
