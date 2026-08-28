# D10 — Zone data flows through Raft; quorum loss makes the control plane read-only

Confirmed as specified in [D19](d19-journal-as-command-log.md). There is one log.
[D24](d24-what-the-cluster-replicates.md) corrects what travels along it, and
[D25](d25-cluster-shape.md) says how many nodes there are and what to do with two.

**Accepted consequence:** losing Raft quorum blocks zone edits cluster-wide. It does **not**
affect DNS service: every node keeps answering from its local snapshot, which is the plane
that matters. A DNS cluster that stops answering queries because a control-plane majority is
unreachable would be unacceptable; one that stops accepting edits for a few minutes is
merely inconvenient.

The alternative (per-zone leadership or eventual consistency between nodes) is more
available but gives up the single ordered history that time travel and IXFR rest on. Not
worth it.
