# D35 — Under load, a client without a cookie is refused rather than rate limited

- Decided: 2026-09-02
- Amends: [D23](d23-reflection-is-bounded.md)

## Context

D23 left two things for later and put them in order: DNS Cookies first, Response Rate
Limiting second. Its closing sentence about RRL says something stronger than "second":

> That is what makes it a policy question with knobs rather than a switch, and it is why it
> does not belong in a product whose point is that nothing needs configuring.

The roadmap in the conventions lists RRL as coming anyway. Two committed texts disagree, and
the disagreement has to be settled before cookies are built rather than after, because what
cookies are for depends on the answer. If RRL follows them, cookies are an identification
feature and the reflection defence is the rate limiter behind them. If it does not, cookies
are the reflection defence and have to be built as one.

The spec is less comfortable than D23's sentence. RFC 7873 §5.2.3 gives a server three
choices for a request carrying a Client Cookie only: discard it silently, answer BADCOOKIE,
or answer it normally, "based on server policy, including rate limiting". §9 adds that
servers "need to take other measures, including rate-limiting responses". Cookies are not
offered as a replacement for a rate limiter anywhere in the document that defines them.

## Decision

**While the server is under load, a query carrying no valid Server Cookie is refused with
BADCOOKIE rather than answered. The rate is still not policed.**

That is the second of §5.2.3's three choices, settled here rather than left to each
deployment. It is one condition and one response: no per-response-class accounting, no SLIP
fraction, no exempt list, nothing an operator is asked to pick a number for. D23's objection
to RRL was that it is knobs rather than a switch, and this is the switch.

It also fails in a different direction than RRL does. Rate limiting by traffic shape has
false positives by construction, and the resolver behind a busy address that looks like an
attack is a real client whose queries get dropped. Refusing a cookieless client costs that
client one extra round trip, once, after which it holds a Server Cookie and is never refused
again. The client that cannot pay the round trip is the spoofed source, which is the one this
is aimed at: it never receives the BADCOOKIE, so it never returns, and the reflection it was
buying is a response close in size to the query it forged.

**RRL is not adopted, and it is not closed either.** It comes off the roadmap as a scheduled
item, because scheduling it says the answer above is known to be insufficient and it is not.
What the answer leaves exposed is clients that implement no cookies at all, which this server
cannot tell from a spoofer by anything except traffic shape. That is the case RRL exists for.
Reopening it takes a record, and that record starts with a measurement of how much of that
traffic there actually is, not with the observation that BIND, Knot and NSD all ship one.

**What "under load" means is the one question this record does not close.** The threshold has
to be derived from something the server already knows rather than configured, or the knobs
come back through the other door. Answering cookieless queries normally until a limit is
reached is what makes the first round trip free in the ordinary case, so the limit cannot
simply be dropped either.

## Consequences

Cookies stop being an identification feature and become the reflection defence, which raises
what they have to get right. RFC 9018 §4.4 pins the construction, SipHash-2-4 is mandatory by
its §6, and the option is exactly 24 octets. The timestamp sub-field is what makes secret
rotation possible, so rotation is part of building this rather than a later thought.

`TestAmplificationFactor` in `internal/dns/wire_test.go` gains a row. A BADCOOKIE response is
the smallest thing this server sends to a query it understands, and pinning its factor is
what keeps that true.

The scope fence names RRL and cookies together in one line, on the strength of D23 covering
both. That line splits. Cookies stay outside it the way clustering and Postgres do, as work
not started rather than work refused, and cross when they are built. RRL is outside it for a
different reason now, and the fence says which.
