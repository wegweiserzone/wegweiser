# D12 — Performance targets

| Metric | Target |
| --- | --- |
| Records, all zones | 1,000,000 |
| Records, largest single zone | 250,000 |
| Query throughput | 50,000 qps on commodity hardware |
| Query latency, in-memory hit | p99 < 1 ms |
| Commit to visible in the data plane | < 200 ms |
| Cold start to serving, 1M records | < 30 s |
| Allocations per query, steady state | 4, and none of them in the resolver |

The full-zone snapshot rebuild (`dns.Rebuild`, in `internal/dns/build.go`) is acceptable only
while it meets the commit-to-visible target at 250k records in a zone. Missing it makes the
incremental copy-on-write rebuild required rather than optional.

**Where this stands: of the seven rows, one is missed, one is part-way proven and five are
estimates.**
They were written as release gates before anything existed to measure them against. What
`go test -bench` shows today, on a Ryzen 7 7700 over loopback:

| Row | Where it stands |
| --- | --- |
| Query latency | `Snapshot.Resolve` takes 27–36 ns and a full UDP exchange 1853 ns in parallel. Far inside a millisecond, but these are means: no p99 under load has been taken. |
| Allocations per query | Met, against a target this entry lowered. See below. |
| Query throughput | Unmeasured. The parallel benchmark implies room far past 50,000 qps, but it is an in-process loop over loopback rather than a load test, and loopback has no NIC to run out of. |
| Records at 1M / 250k | Unmeasured. Nothing here has been run against a database that large. |
| Commit to visible | Unmeasured at 250k records in a zone, which is the size the rebuild question turns on. |
| Cold start | Unmeasured. |

Measuring the rest needs a seeded database of a million records and a generator that keeps
many queries in flight, neither of which exists. Until it does, most of this table is an
estimate and should not be quoted as anything else.

**The allocation target moved from zero to four, deliberately.** It was written before
anything could be measured, and measurement says it is aimed at the wrong thing. A query
costs about 9.3 µs of CPU in this process, of which roughly 0.45 µs is our code and the rest
is `recvfrom` and `sendto`: the kernel is around 95 % of the bill. The four allocations are
128 octets in the message layer, two of them because the wire library decodes the name into
presentation form and `ParseName` encodes it straight back.

Removing three of them means a hand-written wire-format name parser, in the one code path
where a malformed packet must never panic and which is fuzzed for exactly that reason. That
is new code with a new failure mode, spent on 5 % of a query's cost, to make a number look
better. The resolver allocating nothing is the property worth holding, and it holds. If a
benchmark on a real network interface ever shows allocation pressure mattering, the note in
`internal/dns/wire.go` says what to do about it.
