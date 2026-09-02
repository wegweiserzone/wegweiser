# Decisions

Every settled question about how Wegweiser works, one record each. A record states the
decision, the reasoning behind it, and what it obliges the implementation to do. The point is
that the next person does not re-litigate it from scratch.

The code cites these by number: a comment reading `(D3)` or `docs/decisions/ D11` means the
record below. Cite them the same way in a commit or an issue.

## How a record works

**The decision and the reasoning are fixed.** They were true when they were taken, and a
record is not edited to keep up with the world. Where a record carries a `Decided:` date, that
is when the argument was had.

**`Where this stands` is the part that is maintained.** It says what is actually built, which
is the only thing that moves. A record with no such section describes something that is
simply in force.

**A reversal is a new record, not an edit.** It supersedes the old one, the old one stays, and
the two link to each other. `Amends:` and `Amended by:` in the header are how that reads.

Anything settled belongs here. A change to how the system is put together gets a record
before it gets a commit; see [CONTRIBUTING.md](../../CONTRIBUTING.md). Keep one under a page.

## The records

| | |
| --- | --- |
| **D1** | [Module path](d01-module-path.md) |
| **D2** | [SOA serials increment by one, always](d02-soa-serials.md) |
| **D3** | [PTR conflicts default to first-wins, and are never silent](d03-ptr-conflicts.md) |
| **D4** | [Overriding a generated record detaches it](d04-detaching-generated-records.md) |
| **D5** | [Named tokens with scopes; the GUI uses a session cookie](d05-tokens-and-sessions.md) |
| **D5a** | [One delegation rule, and it is the strict one](d05a-delegation-rule.md) |
| **D6** | [Reverse zones are offered, never auto-created](d06-reverse-zones-are-offered.md) |
| **D7** | [RFC 2317 CNAMEs are generated when we hold the parent](d07-rfc2317-cnames.md) |
| **D8** | [Journal retention is unlimited by default](d08-journal-retention.md) |
| **D9** | [The query stream filters server-side, then samples, and says so](d09-query-stream-sampling.md) |
| **D10** | [Zone data flows through Raft; quorum loss makes the control plane read-only](d10-quorum-loss-is-read-only.md) |
| **D11** | [The config file holds bootstrap settings only](d11-config-holds-bootstrap-only.md) |
| **D12** | [Performance targets](d12-performance-targets.md) |
| **D13** | [DCO, not a CLA](d13-dco-not-cla.md) |
| **D14** | [QTYPE=ANY is answered with one RRset](d14-any-returns-one-rrset.md) |
| **D15** | [The metrics use the Prometheus client library, and cost 5 MB for it](d15-prometheus-client-library.md) |
| **D16** | [The web interface lives in this repository, embedded, and can be switched off](d16-web-interface-in-repo.md) |
| **D17** | [This server does not resolve; a resolver goes in front](d17-no-recursion.md) |
| **D18** | [Store RDATA in canonical presentation format](d18-rdata-presentation-format.md) |
| **D19** | [The journal is the replication log](d19-journal-as-command-log.md) |
| **D20** | [Store individual records, enforce RRsets in validation](d20-individual-records.md) |
| **D21** | [Generated records are materialized, with provenance](d21-materialized-managed-records.md) |
| **D22** | [internal/zone depends on the wire library](d22-zone-uses-the-wire-library.md) |
| **D23** | [Reflection is bounded by construction, and the rate is not policed](d23-reflection-is-bounded.md) |
| **D24** | [What the cluster replicates](d24-what-the-cluster-replicates.md) |
| **D25** | [Cluster shape: a few voters, and everything else replicates](d25-cluster-shape.md) |
| **D26** | [Outbound zone transfer: who may ask, and what is answered](d26-outbound-zone-transfer.md) |
| **D27** | [NOTIFY: who gets told, and when](d27-notify.md) |
| **D28** | [TSIG: a transfer is granted to a key, not to an address](d28-tsig.md) |
| **D29** | [A node that cannot apply an entry leaves the cluster and keeps answering](d29-a-node-that-cannot-apply.md) |
| **D30** | [What a log snapshot contains](d30-what-a-log-snapshot-contains.md) |
| **D31** | [A zone check reports refusals and diagnoses, and says which is which](d31-what-a-zone-check-reports.md) |
| **D32** | [What the cluster replicates besides zone data](d32-what-else-the-cluster-replicates.md) |
| **D33** | [A conflict is derived, not stored](d33-a-conflict-is-derived.md) |
| **D34** | [The secondary's configuration is generated, not installed](d34-generated-secondary-configuration.md) |
| **D35** | [Under load, a client without a cookie is refused rather than rate limited](d35-cookieless-under-load.md) |

D18 through D28 were written as architecture decision records in a directory of their own.
They are the same kind of document and were folded into one series, keeping the dates they
carried. D5a carries a letter rather than a number of its own, and that is all the letter means: it
is not an extension of D5, whose subject is unrelated to it.

## Open, not decided

- **D12's numbers need a benchmark to be real.** They are informed estimates until the harness
  exists, and building it is part of the first performance-relevant milestone.
- **Wire-format response caching** for hot RRsets is an obvious optimisation and is
  deliberately not designed yet. It only earns its complexity if the allocation target in D12
  turns out unreachable without it.
