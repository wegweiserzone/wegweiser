# D3 — PTR conflicts default to first-wins, and are never silent

- Amended by: [D33](d33-a-conflict-is-derived.md)

When a new A/AAAA record points at an address that already has a managed PTR, the existing
PTR stays. A conflict record is produced, returned by the API, and surfaced in the GUI with a
one-click "make this the canonical name" action.

Configurable globally and per zone: `first-wins` (default), `last-wins`, `multi`, `reject`.

Several names pointing at one address is the normal case: virtual hosts, a load balancer, a
service alias. `multi` would be the most literal reading of "generate the PTR", but it turns
a routine operation into a five-entry PTR RRset, which breaks the near-universal expectation
that a reverse lookup yields *the* canonical name and upsets reverse-lookup-based mail
checks, logging and access control. `reject` would fail a write for a reason that is not the
user's problem. `first-wins` is the only option that never changes an answer the user did not
ask to change.

**Obligation.** A conflict is a first-class object rather than a log line: it is returned in
the API response, listed under the zone, and clearable. A conflict that is only visible in
the server log is the same as no conflict detection at all.

**Where this stands: built, except per zone.** Every write that hits a conflict reports it,
in the API response and in both clients, and it carries the policy that decided it. Listing
went the way [D33](d33-a-conflict-is-derived.md) settled: nothing stores a conflict, and the
zone's check reports each one as a warning, in `weg zone check --reverse` and on the check
screen of the GUI. Clearing was dropped there too. "Make this the canonical name" is
`weg record canonical`, and the GUI offers it beside the finding.

The policy is one server-wide setting, `weg settings set --reverse-conflict-policy`. A zone
cannot carry its own. Making a name canonical is the one write that overrides it, because
taking the entry from whoever holds it is what that action asks for.
