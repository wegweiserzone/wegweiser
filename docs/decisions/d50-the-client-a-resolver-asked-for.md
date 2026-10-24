# D50 — The client a resolver asks for is shown, when the resolver is one this server believes

- Decided: 2026-10-24

## Context

[D17](d17-no-recursion.md) settles that this server does not resolve, and says what to run
instead: a resolver in front, which takes the clients' queries and hands the ones for these
zones down. In that arrangement every query arrives from the resolver. The query stream then
reads `127.0.0.1` on every row, and the question it exists to answer, which machine keeps
asking for that name, cannot be answered from it.

A resolver can say who it is asking for. EDNS Client Subnet (RFC 7871) is an EDNS option
carrying the client's address, or a prefix of it, and Unbound sends it to the servers it is
told to (`send-client-subnet`). It was made so that an authority could tailor its answer to
where the client is.

## Decision

**The option is supported as RFC 7871 asks of an authority, on every query that carries it.**
It is read and checked: an unknown family, or an address longer or shorter than its source
prefix calls for, is answered FORMERR (§6, §7.2.1). A response to a query that carried it
carries it back with the family, source prefix and address unchanged and a scope prefix of
0 (§7.2.1, §12.1), delegations included (§7.4). Scope 0 is the truth: no answer here depends
on where the client is, and it is what tells a resolver's cache that the answer holds for
everybody. Reading the option and not returning it is not an option; a server that does not
support ECS has to ignore it entirely (§7.2.1).

**What the option says about the client is believed only from a resolver on a list.** The
option is the sender's word, and a sender can write any address into it. The list names the
addresses whose word is taken, it is a setting like the transfer list
([D32](d32-what-else-the-cluster-replicates.md)), replicated with it, and it starts empty. A
query from anywhere else is answered exactly the same and observed by its own address.

**It is shown and nothing else.** The exchange keeps the address it arrived from, and gains
the client it was asked for, as the prefix the resolver sent: the whole address where the
resolver sends a /32 or /128, a network where it truncates, as RFC 7871 §11.1 encourages.
A source prefix of 0 is a client that asked not to be named (§7.1.2), and none is shown.
Nothing about the answer, the cookie check of [D35](d35-cookieless-under-load.md) or the
transfer list looks at it: answering by who asks is views and split-horizon, which the scope
fence keeps out.

**The stream's client filter matches either.** Watching a network
([D9](d09-query-stream-sampling.md)) shows its queries whether they came directly or through
a resolver that named them, because that is the question somebody filtering by a network is
asking.

## Consequences

The response grows by the option the query brought, octet for octet, at most 24. A query has
to carry as much as it gets back for it, so the amplification factor
[D23](d23-reflection-is-bounded.md) measures does not rise, and the 1232-octet ceiling on a
UDP response holds as before.

A resolver keeps its cache shared. Given scope 0, Unbound stores the answer for every client,
as it would without the option, so turning `send-client-subnet` on costs it nothing but the
option on the wire.

The setup lives on the resolver's side: Unbound needs its subnet module and the full prefix
lengths (`max-client-subnet-ipv4: 32`, `max-client-subnet-ipv6: 128`) for whole addresses.
The page on a resolver in front says how.

## Where this stands

Built as described. The option is handled in `internal/dns/subnet.go`, the list is the
`resolvers` setting (`weg settings set --resolvers`, and a panel on the settings page), and
the client is `for` in a query event, shown in `weg query tail` and on the query stream.
