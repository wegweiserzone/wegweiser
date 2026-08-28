# Security Policy

Wegweiser answers unauthenticated queries from the network, so a bug in it can reach people
who never chose to run it. Reports are welcome. I would rather hear about a problem than not.

It is written and maintained by one person in their spare time. The timings below are honest
estimates, not a service level.

## Supported versions

Fixes go into the current release. Older ones get nothing: there is one person here, and
backporting to a version nobody runs is time taken from the version people do run. Whether
the fix ships as a patch or as a minor release depends on what it had to touch. If you are
running something older, upgrading is the fix.

## Reporting a vulnerability

Please **do not open a public issue.**

Use GitHub's private reporting:
[Security → Report a vulnerability](https://github.com/wegweiserzone/wegweiser/security/advisories/new).

What helps in a report:

- what an attacker gains, and what access they need before they can start
- something to reproduce it with: a packet capture, a `dig` invocation, a short program
- the affected version (`weg version --output json`) and the platform

I usually answer within a few days. If two weeks pass with nothing back, the report has gone
astray: open a public issue saying only that you are waiting on a security report, and I
will pick it up.

Fixes are written in private. Reporters are credited on release unless they would rather not
be named. The disclosure timeline is yours to set: if you tell me you are publishing on a
given date, that date holds, fix or no fix.

Anything already known is published as an advisory, so the
[advisory list](https://github.com/wegweiserzone/wegweiser/security/advisories) is worth a
look before you write. The release that carries a fix names it in the
[changelog](CHANGELOG.md).

## In scope

- Crashes, panics or hangs reachable from network input. A malformed DNS packet must never
  panic, which is what the wire parser's fuzz targets are there for
- Answering authoritatively for names outside a configured zone, or claiming NXDOMAIN
  outside of authority
- Cache-poisoning-adjacent behaviour: response mismatches, ignored 0x20 encoding,
  predictable transaction identifiers
- Amplification past the bounds the query path is supposed to hold: a response that ignores
  the UDP size ceiling, an EDNS0 buffer that never gets clamped
- Authentication and authorization flaws in the API: token handling, scope bypass, session
  fixation, CSRF
- Privilege escalation, or any code path that wants root when nothing here should
- Stored zone data that breaks out of where it belongs, into the wire format, into an
  exported zonefile or into the web UI

## Out of scope

- Query floods. The amplification factor is bounded by construction and pinned by a test;
  [D23](docs/decisions/d23-reflection-is-bounded.md) has the measured numbers and
  what the server does about the rate. Beat those bounds and it is a finding. Volume alone
  against an unprotected instance is not.
- Anything that needs the admin token or filesystem access before it works
- Missing hardening headers on the API when it is deliberately run behind a reverse proxy
- Scanner output with no demonstrated impact
- Anything the server does not implement yet, which is a feature request rather than a
  vulnerability. The [scope fence](docs/conventions.md#scope-fence) is the list of what is in
  and what is out, and it moves with the code. Recursion is the one worth naming here:
  Wegweiser does not resolve, by design
  ([D17](docs/decisions/d17-no-recursion.md)), so a name outside its zones is
  REFUSED rather than looked up. That is the intended answer, not a gap.

## Notes for operators

Wegweiser is designed to run unprivileged. It binds port 53 through
`CAP_NET_BIND_SERVICE` and never requires root, and the systemd unit that ships with it is
sandboxed to match. A deployment running it as root is a misconfiguration rather than a
requirement. If some documentation led you there, report that too.

Two things are worth a moment at install time. Every release ships a `checksums.txt` over its
artefacts, and checking it is one command ([README](README.md#install)). And the API is
served on loopback by default for a reason: a token on it can change every zone this server
answers for, so putting it on a network is a decision to make deliberately, with TLS in
front of it.
