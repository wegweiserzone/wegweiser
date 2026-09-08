# D38 — Under load is what the kernel says it is

- Decided: 2026-09-08
- Amends: [D37](d37-under-load-is-when-the-readers-stop-idling.md)

## Context

D37 derived the state from the readers' own idleness: while any of them still waits for a
datagram, queries are arriving more slowly than this machine answers them. It was built, and
then it was run against a real server before a release, which is where it fell over.

One reader, pinned to one core, was offered 2.2 million datagrams a second. The kernel was
discarding them, and a probe query needed four attempts to be answered at all.
`weg_dns_under_load` stayed at zero throughout, and a cookieless query was answered normally
in the middle of it.

Instrumenting the windows says why:

| Traffic | Idleness the window measured |
| --- | --- |
| Quiet | 0%, the reader being inside one receive call for the whole second |
| 1,800 queries a second | the whole window |
| 2.2 million offered | 15.5%, which is 155 ms a second |

Those 155 ms are not waiting. They are the microseconds a receive call costs, added up over
a few hundred thousand datagrams that were already queued when it was made. A clock around
that call measures how long the call took, and D37 read it as how long the reader waited.
The two are the same number only when the socket is empty.

## Decision

**Under load is the kernel having dropped a datagram on one of the server's sockets during
the window.**

`getsockopt(SO_MEMINFO)` answers with nine counters for a socket, the last of which is
`sk_drops`. One call per socket per window, made on the goroutine that already decides the
state, so nothing is measured on the query path at all. Measured on the same machine, on a
socket nobody was reading from:

```
quiet:    queued=0       rcvbuf=212992  drops=0
flooded:  queued=212160  rcvbuf=212992  drops=94238
```

**D37 rejected this as the same signal seen too late.** The measurement corrects that
sentence rather than the ordering it assumed. The earlier signal is not earlier, it is
absent, and a state that arrives one window after the machine began losing queries is what
there is to have.

**Where the kernel will not answer, nothing is refused.** An option an older kernel does not
implement leaves the server exactly as it behaved before cookies: it answers everyone, and
the switch never closes. That is reported once, through the fault hook the server already
has, rather than every window at anybody watching.

**The state oscillates, and it is a control loop rather than a latch.** A refused exchange
costs less than an answered one, so a rate that overwhelmed the server may stop overwhelming
it while the refusals are in force, at which point the drops stop and the next window is not
under load. A client refused in one second is answered in the next. An attacker whose
traffic sits just above what this machine serves has its reflection cut rather than stopped,
which is worth knowing before somebody reads the switch as a wall.

**Rejected: a threshold on the idleness above.** Fifteen per cent against a hundred is a wide
gap, and a number in between would work on the machine it was measured on. It is the knob
D23 and D35 both refuse, it belongs to one processor and one network interface, and the
first server it is wrong on is a server nobody tested. **Rejected too: the depth of the
receive queue**, which shows a reader falling behind before a drop does, but only means
"never emptied" if it is sampled several times a window, and a sampling rate is another
number to pick.

## Consequences

`internal/dns/load.go` gets smaller. The per-reader counters go, the two clock reads per
datagram go with them, and the meter becomes a loop over the sockets the server bound. The
query path returns to what D12 measured, which is one clock read per exchange and only where
somebody is watching.

The switch becomes something `go test` can close. A socket with a small receive buffer,
filled and not read from, drops datagrams in milliseconds, and that is the entire condition:
no load generator, no timing luck, no sleeping for a second to see what happened.

Nothing above the meter moves. `weg_dns_under_load`, `weg_dns_cookie_refusals_total`, the
refusal in the message layer and everything D35 settles stay as they are. What changes is
where the state comes from.

## Where this stands

Built, and measured against the server that showed up the mechanism it replaces. One reader
pinned to one core, offered 2.2 million datagrams a second: `weg_dns_under_load` reads 1
throughout, a query with no cookie is REFUSED, one with a Client Cookie only is BADCOOKIE
with a Server Cookie in it, a client holding a valid cookie is answered, and TCP is answered
whatever the load. The state falls back to 0 within a window of the traffic stopping, and
cookieless queries are answered again.

Refusing is cheaper than answering, which the same run shows: about six million refusals in
twenty seconds, against a server that answers something over a hundred thousand queries a
second. `BenchmarkServerUDP` is where it was before D37, at 11.0 µs serial and 1.87 µs
parallel, because the clock reads that record added are gone again.
