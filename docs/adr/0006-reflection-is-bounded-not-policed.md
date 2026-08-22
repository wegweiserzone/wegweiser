# ADR 0006 — Reflection is bounded by construction; rate limiting is v0.2

- Status: accepted
- Date: 2026-08-18

## Context

An authoritative DNS server on a public address answers whoever asks, and UDP lets whoever
asks lie about who they are. A spoofed query therefore turns this server into a reflector
pointed at a third party, and what makes that worth an attacker's trouble is the
amplification factor: how many octets of attack traffic one octet of their own buys.

The standard mitigations are Response Rate Limiting (BIND's `rate-limit`, Knot's `rrl`,
NSD's `rrl-ratelimit`, all descendants of Vixie and Schryver's 2012 design) and DNS Cookies
(RFC 7873, RFC 9018), which let a server tell an off-path spoofer from a client that has
proved it can receive what it is sent.

Neither is in the v0.1 scope fence, and neither had been ruled on. That is the gap this
records: not that they are missing, but that nobody had decided whether they should be.

## Decision

**v0.1 bounds the factor and does not police the rate.** RRL and DNS Cookies are v0.2.

The bounds already in the query path are there for this, and they are what makes the
decision defensible rather than merely convenient:

- **1232 octets** is the ceiling on a UDP response, whatever a client advertises
  (`Limits.MaxUDPResponse`). Without EDNS the cap is the 512 of RFC 1035 §4.2.1.
- **One RRset answers ANY** (RFC 8482). The classic lever (one small question, everything
  at a name) returns no more than asking for a type directly would.
- **Sixteen records** is the most the additional section is filled with.
- **A name we do not serve is REFUSED**, not looked up: the cheapest possible answer.

Measured, and pinned by `TestAmplificationFactor` in `internal/dns`:

| Query | Response | Factor |
| --- | --- | --- |
| TXT, no EDNS | 299 octets | **9.6×** |
| TXT, EDNS 4096 | 1114 octets | **26.5×** |
| ANY, EDNS 4096 | 1114 octets | **26.5×** |
| A name we do not serve | 87 octets | **2.1×** |

26.5× is the worst this server has, and reaching it needs a large RRset in a zone it already
hosts. For scale: an open resolver with a zone crafted for the purpose reaches 50–100×, and
NTP `monlist` reached 500×. A factor in the twenties makes this a poor reflector, though
still a usable one.

## Consequences

Accepted knowingly: **a v0.1 node on a public address can be used to reflect traffic at a
third party**, at up to 26.5× and only while the attacker has a large RRset to aim at. An
operator exposing v0.1 to the internet should put a rate limiter in front of it. On Linux
that is one nftables rule:

```
nft add rule inet filter input udp dport 53 limit rate over 200/second drop
```

The number is pinned by a test. Raising `MaxUDPResponse`, loosening the additional-section
bound or answering ANY with everything at a name are all allowed; doing any of them without
noticing what it does to this factor is not.

What v0.2 takes on, in this order:

1. **DNS Cookies (RFC 7873/9018)** first. It is bounded work with a precise specification,
   it costs one HMAC per query, and it identifies a spoofed source rather than guessing at
   one from traffic shape. A client with a valid cookie is known to be reachable.
2. **RRL** second, and only for cookieless clients. Rate limiting by traffic shape has false
   positives by construction (a legitimate resolver behind a busy address looks like an
   attack) so the SLIP behaviour that answers a fraction of limited queries with TC set,
   pushing a real client to TCP, is not optional. That is what makes it a policy question
   with knobs rather than a switch, and it is why it does not belong in a release whose
   point is that nothing needs configuring.

Neither changes the query path's shape: both sit in the message layer between reading a
datagram and resolving it, which is where the transport is still known.
