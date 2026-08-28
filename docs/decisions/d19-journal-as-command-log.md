# D19 — The journal is the replication log

- Decided: 2026-08-01
- Amended by: [D24](d24-what-the-cluster-replicates.md)

## Context

Two of the four product differentiators depend on a write log. Time travel needs an ordered
history of changes to a zone; the cluster needs an ordered history of changes to replicate.
Raft (`hashicorp/raft`) is the fixed choice for the control plane, and Raft is itself a
replicated log with a state machine applying entries.

The obvious implementation, taken independently, produces two logs: an application journal
in SQL for audit and IXFR, and a Raft log for replication. That is the classic distributed
systems failure: two logs that must agree, with no mechanism forcing them to. Every ordering
bug, every partial failure and every restart is a chance for them to diverge, and divergence
in a DNS server means two nodes serving different answers for the same name.

Raft is not built. The decision is not deferred with it, because it constrains the shape of
the write path that is.

## Decision

There is one log. The journal **is** the replicated log.

The write path is structured as a state machine from the start:

```
Command  ──validate──▶  []Event  ──apply──▶  Store + Snapshot
(intent)                (facts)
```

- A **`Command`** is client intent: "add this record to this zone". It is deterministic
  input, serializable, and it is the unit Raft will replicate.
- **Validation and reverse-automation expansion happen once, on the leader**, before the
  events are produced. They read current state and must not be re-run per node.
- **`[]Event`** are the resulting facts: record-level adds and deletes plus a serial step.
  Applying them is deterministic and requires no state beyond the events themselves.
- The **applier is already an FSM**: `Apply(commit) error`, no hidden inputs, no clock
  reads, no randomness.

What the cluster phase adds is a transport in front of the applier and a leader check. It
does not add a second log, and it does not restructure the write path.

## Rationale

Replicating `Command` rather than `[]Event` was rejected: validation depends on current
state, so two nodes applying the same command at different points in their history could
reach different results. Replicating already-expanded events makes application deterministic,
which is exactly the property Raft requires of a state machine.

The two-log alternative was rejected because there is no cheap way to make two logs agree.
The expensive ways (two-phase commit between Raft and SQL, or reconciliation on startup)
are more code than doing it right initially, and are only exercised during failures, which
is where untested code lives.

Consistency model, stated explicitly since it is a real trade: Raft gives a single write
leader and blocks writes when quorum is lost. Reads, DNS queries, keep being served from
each node's local snapshot regardless, so a quorum loss degrades to "the cluster is
read-only", never to "the cluster stops answering". For a DNS server, where the data plane
is what matters and the control plane is edited by humans at human rates, that is the right
side of the trade.

## Consequences

- `Command` and `Event` must be serializable and version-tolerant from the first commit. A
  node running a newer binary will replicate to one running an older binary during a rolling
  upgrade, so an unknown field must not be fatal. Encoding choice is deferred, but
  additive-only evolution is a rule from now on.
- The applier takes no ambient dependencies: no globals, no `time.Now()` inside, no random
  IDs generated during apply. Timestamps and IDs are assigned when the command is accepted
  and travel with it.
- Journal truncation becomes a cluster concern later: a lagging follower needs the entries a
  leader might otherwise compact. Checkpoints (`zone_checkpoints`) are the snapshot-transfer
  mechanism for that case, which is why they exist already even though single-node rollback
  could manage without them.
- A cost is paid now: the write path is split into command / validate / expand / apply
  when a direct `INSERT` would work. This is deliberate and is the only place where the MVP
  builds for a deferred feature: justified because retrofitting it is a rewrite of every
  write path, not an addition.
