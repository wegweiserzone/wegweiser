# D49 — Asking the server what it answers, over the API

- Decided: 2026-10-12

## Context

Finding out what this server answers for a name means a DNS client: `dig`, pointed at the
right address and port, from a machine that can reach the DNS listener. The person in the web
interface has a browser, and often none of that. The question they bring is the one that
matters most when something is wrong ("what does it say for `www`, and why"), and the
interface could not answer it.

The query path decides an answer in two steps. [`Snapshot.Resolve`](../../internal/dns/resolve.go)
is a pure function of the published snapshot and a question; it chooses the response code,
the AA bit, the three sections and the extended error of RFC 8914. The message layer then
encodes that, and adds what depends on the client and the transport: the casing the client
used, truncation to the UDP size it offered, and the refusal of a query without a cookie while
the server is under load ([D35](d35-cookieless-under-load.md)).

## Decision

**`GET /ask` answers a question by calling `Resolve` on the snapshot the query path is
serving, and returns what it decided.** Not a second implementation of the search, and not a
DNS query sent to the server's own socket: the first would drift from the real answer, and
the second would answer from whichever listener the API process could reach, through the
load shedding meant for strangers.

**It answers as a query over TCP would be answered.** What the message layer adds belongs to
a client the API does not have: there is no casing to echo, no UDP size to fit, and no
cookie to check. Everything `Resolve` decides is in the answer unchanged, the extended error
included, because that text is the server's own explanation of a response.

**It says which zone answered, and what kind of answer it was**: an answer, an alias, no data,
no such name, a referral, refused, or not implemented. Those follow from the response code,
the AA bit and the sections, by the rules of RFC 1034 §4.3.2 and RFC 2308. Naming them saves
every client from working them out again, and keeps the web interface and `weg ask` saying the
same thing.

**It needs the read scope, like any other read.** It reveals nothing a read of the zone's
records does not, and a session that may only read is exactly the one that most needs to ask.

## Consequences

The answer is the member's own. In a cluster, a member answers from its copy, as it does on
the wire, so asking a member that trails the leader shows what that member answers, which is
the point when a member is the one in question.

A name with no zone on the server is answered as the query path answers it: refused, with the
reason. It is not an error of the request.

## Where this stands

Built as described, as `GET /ask`.
