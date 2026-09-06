# D37 — Under load is when the readers stop idling

- Decided: 2026-09-06
- Closes the question left open by [D35](d35-cookieless-under-load.md)

## Context

D35 refuses a query carrying no valid Server Cookie while the server is under load, and says
plainly that it does not settle what under load is:

> The threshold has to be derived from something the server already knows rather than
> configured, or the knobs come back through the other door.

Both halves of that bind. A number in the configuration file is the thing D23 and D35 both
refuse, and dropping the condition is not available either: answering cookieless queries
normally until a limit is reached is what keeps the first round trip free for a client that
has never spoken to us before.

What the server knows about itself is a count of queries, the size of what it sent, and how
long each exchange took. None of that is a threshold. A rate becomes one only against a
second number, and the second number here is the capacity of this machine, which nobody has
measured and which no operator should be asked to guess.

## Decision

**The server is under load when its datagram readers stop idling.**

A reader is a loop: block on a receive, answer what came back, block again. The blocking is
the signal. As long as a reader ever waits for a datagram, queries are arriving more slowly
than this machine answers them, whatever the rate happens to be. A period in which no reader
waits at all is the arrival rate having overtaken the service rate, and it says so without
anybody choosing a number. The capacity is whatever the hardware and the socket count add up
to on the day.

**The state is evaluated on a fixed cadence of one second**, across all readers together. It
is entered when a whole second passes with no reader idle, and left at the first second that
has some. The second is a cadence, in the way a scrape interval is: it decides how quickly
the switch follows the traffic, not who gets refused, and no deployment has a reason to
prefer another one.

**It applies to the datagram path only.** A query over TCP is never refused for want of a
cookie. That client completed a handshake, which is the proof of address a cookie exists to
obtain, and the idleness of the datagram readers says nothing about it either way.

**A query carrying no OPT at all is refused with REFUSED, not BADCOOKIE.** RFC 6891 §6.1.1
forbids an OPT in the response to a request that carried none, and BADCOOKIE is an extended
code that lives in that OPT. A client speaking no EDNS can be handed no cookie and told
nothing about cookies, so under load it gets the smallest honest thing this server can say.
The EDNS client without a valid Server Cookie gets BADCOOKIE with one to come back with,
which is D35's round trip.

**Nothing is counted per source.** No table of addresses, no exemption list, no per-response
class accounting. One condition about this machine, and one response to it.

## Consequences

The trigger is a property of the machine, so the same flood is refused on a small node and
answered on a large one. That is the intent rather than a side effect: the moment we cannot
serve everyone is the moment the queries we cannot attribute go first, and until then nobody
pays for the defence.

**A node with headroom remains a usable reflector.** Traffic can be large enough to hurt a
third party at 26.5× and small enough to leave our readers idling, and this switch never
trips on it. D23's answer holds underneath: the factor is bounded by construction, and an
operator exposing the server to the internet puts a rate limiter in front. RRL is still
reopenable on D35's terms, which start with a measurement rather than with this gap.

Idleness has to be measured where the reader loops, which is the hot path. A clock read or
two per datagram is tens of nanoseconds against the 9.3 µs D12 measured, and against the
0.45 µs of it that is our own code it is not free. The cheaper form is a flag the loop sets
and a sampler that reads it; which one is built is a question for the code, not for this
record.

The switch is testable without a load generator, which a rate threshold is not. Readers can
be held busy in a test and the state observed directly, so the refusal path is exercised by
`go test` rather than by argument.

An operator has to be able to see it. The state is a gauge and the refusals are a counter,
and the query stream already carries the rcode of every exchange, so a BADCOOKIE burst is
visible as it happens rather than after the fact.

**In a cluster, every node decides for itself.** This is the state of one machine's sockets.
A node with a quiet interface has no reason to refuse anyone because another node is busy, so
nothing here joins the list in D32.

## Where this stands

Nothing of it is built. Cookies are not built either, and this record is what that work
starts from.
