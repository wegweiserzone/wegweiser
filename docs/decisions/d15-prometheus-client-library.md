# D15 — The metrics use the Prometheus client library, and cost 5 MB for it

`github.com/prometheus/client_golang`, not a hand-written exposition format. It brings the
protobuf runtime, `procfs` and the rest of its tree with it: the binary went from 15.1 MB to
20.2 MB, a third larger.

The alternative was writing the text format ourselves. It is not hard (`# HELP`, `# TYPE`,
label escaping, cumulative histogram buckets) but it is about four hundred lines once the
Go runtime and process collectors are included, and those are the ones an operator reaches
for first when answers get slow. Four hundred lines of infrastructure we would own and have
to keep correct, to save five megabytes of a binary that is downloaded once, is the wrong
trade. Nothing about it is on the hot path: the whole cost per query is 87 ns and no
allocations.

**The endpoint needs a credential.** `GET /api/v1/metrics` is authenticated like everything
else; `/healthz` remains the only endpoint that is not, because a load balancer has nowhere
to put a token and a scraper does: Prometheus has had `authorization` in its scrape
configuration for years. What a server is asked, how often, from where and how many zones it
holds is operational detail, and a metrics endpoint is the most detailed description of a
deployment that exists.

**Obligation.** The client library has exactly one importer, enforced by `depguard`.
Everything is fed through `dns.Config.Observe`; a second importer would be a counter
registered on a registry nobody serves. Label values that come from the wire are folded to a
bounded set, so what a client asks for cannot decide how many time series exist.
