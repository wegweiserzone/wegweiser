# D17 — This server does not resolve; a resolver goes in front

- Decided: 2026-08-22

## Context

A client takes its nameservers from DHCP and keeps two or three of them, no more: glibc
reads `MAXNS` entries out of `resolv.conf` and ignores anything past that. If those entries
point at Wegweiser, the client can look up the zones this server holds and nothing else. A
query for a name no zone covers is REFUSED, which is the honest answer and no use at all to
a laptop trying to reach the internet.

That is the practical shape of a question the scope fence carried as open: should an
authoritative server also forward, the way BIND's `forwarders` does? "One box for a small
network" is a real thing to want, and Technitium, a reference point in the conventions, does
both.

The workaround that suggests itself does not work, and why it fails is worth recording,
because it looks like an intermittent fault rather than a misconfiguration. Hand out one
Wegweiser and one public resolver, and the two directions behave differently:

- **Outward.** A query for `example.org` reaches Wegweiser and is REFUSED. glibc's resolver
  reads REFUSED, SERVFAIL and NOTIMP as a server that is not working and moves to the next
  entry, so the name resolves. It costs a round trip on every external lookup.
- **Inward.** A query for `nas.internal.example` reaches the public resolver and comes back
  NXDOMAIN. That is a valid final answer rather than a failure, so the stub stops there and
  never asks the second server. The name does not exist for that client.

Which server a client tries first is not something the network gets to decide. glibc walks
the list in order, `options rotate` shuffles it, systemd-resolved applies its own logic, and
Windows and Android differ again. What an operator sees is internal names that resolve on
some hosts, in some processes, some of the time.

So the choice is a real one: build forwarding with a cache, or require a resolver in front
and document it well enough that nobody falls into the paragraph above.

## Decision

**Wegweiser answers for the zones it is authoritative for, and for nothing else.** It does
not recurse, does not forward, and holds no cache. This is not work placed behind other
work. There is no planned release in which it starts.

RA stays clear in every response. A name that no zone covers is REFUSED, carrying an
extended error that says why (RFC 8914), and never NXDOMAIN.

A network that needs both its own zone and the internet runs a resolver, and the resolver is
what DHCP hands out. Wegweiser sits behind it, reached through a stub zone. Both on one
machine is fine and is the expected small-network case; they differ by port, not by host.
The documentation carries the working configuration.

## Rationale

Four arguments against building it, and they do not weigh the same.

**Two trust levels on one socket.** An authoritative answer is ours and carries AA. A
forwarded one is somebody else's, unvalidated, and holds the lower rank RFC 2181 §5.4.1
defines. Serving both from one listener is where the classic cache-poisoning bugs came from,
not from the cache itself but from data crossing between the ranks. Keeping them apart
inside one process is possible. It is also a permanent source of subtle, security-relevant
bugs.

**D23 would stop meaning what it says.** The 26.5× amplification factor measured there
holds because we choose what goes in our zones. A forwarder reachable by a spoofed source is
an open resolver, and an open resolver aimed at a zone crafted for the purpose reaches 50 to
100×. The mitigation is a client list, and it would have to be mandatory rather than
advisable: an off-by-default switch is not a safety property, because every open resolver on
the internet belongs to somebody who turned something on.

**Another server's latency inside the query path.** Architecture invariant 2 exists so that
answering a query waits on nothing. A cache miss waits on an upstream across the network.

**A cache is mutable state where there is none.** The snapshot is immutable and swapped by
pointer, and invariant 8 says it can always be rebuilt from the store. A cache is neither, so
it would live outside both, and D12's allocation target holds partly because the resolver has
no path that allocates.

Against all that, the alternative is cheap. A resolver in front is six lines of Unbound
configuration on a machine that is already running. PowerDNS, Knot and NSD each ship the
resolver and the authoritative server as separate daemons. BIND is the one that merged them,
and BIND is the usability counterexample the product thesis is written against.

## Consequences

An operator has to run something besides this. That is a real cost, and the documentation
carries it rather than the code: it gains a page that leads with the DHCP trap above,
then gives Unbound on `:53` with `weg serve --listen 127.0.0.1:5353` behind it, reverse zone
included, and the `dig` invocations that show which half is failing.

The query path keeps its shape. No cache, no upstream, no second trust level, so invariants
2 and 8 are untouched and D12 describes one steady state instead of a hit path and a miss
path with different numbers.

RA and the REFUSED branch in `internal/dns` are load-bearing now rather than incidental. The
tests that pin them are testing a decision.

Reopening this takes a record that supersedes this one, and that argument starts at the
mandatory client list rather than arriving there.
