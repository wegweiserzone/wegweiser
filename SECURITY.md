# Security Policy

Wegweiser answers unauthenticated queries from the network, so a bug here can matter to
people who never chose to run it. Reports are welcome and I would rather hear about a
problem than not.

It is written and maintained by one person in their spare time. The response times below are
honest estimates, not a service level.

## Supported versions

Pre-alpha. Nothing is released yet, so nothing is supported yet. Once v0.1 ships, the latest
release gets security fixes.

## Reporting a vulnerability

Please **do not open a public issue.**

Use GitHub's private reporting:
[Security → Report a vulnerability](https://github.com/wegweiserzone/wegweiser/security/advisories/new).

Useful in a report:

- what an attacker gains, and what access they need to start
- a reproducer: a packet capture, a `dig` invocation, or a short program
- the affected version (`weg version --output json`) and platform

I usually reply within a few days. If two weeks pass with no answer, the report has gone
astray: open a public issue saying only that you are waiting on a security report, and I
will pick it up.

Fixes are written privately and credited to the reporter on release unless you would rather
not be named. You set the disclosure timeline, not me. If you tell me you are publishing on
a given date, that date holds whether or not there is a fix.

## In scope

- Crashes, panics or hangs triggered by network input. A malformed DNS packet must never
  panic; the wire parser has fuzz targets and CI runs a short campaign on every push
- Answering authoritatively for names outside a configured zone, or asserting NXDOMAIN out of
  authority
- Cache-poisoning-adjacent behaviour: response mismatches, ignoring 0x20 encoding, predictable
  transaction identifiers
- Amplification vectors: unbounded response sizes, missing EDNS0 buffer clamping
- Authentication and authorization flaws in the API: token handling, scope bypass, session
  fixation, CSRF
- Privilege escalation, or any path requiring root that should not
- Injection into stored zone data that escapes into the wire format or the web UI

## Out of scope

- Denial of service through sheer query volume against an unprotected instance. Rate limiting
  and RRL are planned; raw flooding of a public authoritative server is expected.
- Findings that require an attacker who already has the admin token or filesystem access
- Missing hardening headers on the API when it is deliberately run behind a reverse proxy
- Automated scanner output without a demonstrated impact

## Hardening notes for operators

Wegweiser is designed to run unprivileged. It binds port 53 through
`CAP_NET_BIND_SERVICE` and never requires root. The shipped systemd unit is sandboxed
accordingly. If your deployment runs it as root, that is a misconfiguration rather than a
requirement: please report the documentation that led you there.
