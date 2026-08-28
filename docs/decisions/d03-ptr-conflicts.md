# D3 — PTR conflicts default to first-wins, and are never silent

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

**Where this stands: returned, not yet listed.** Every write that hits one reports it, in the API
response and in both clients, and it carries the policy that decided it. Listing and
clearing are not built, because a conflict is computed during a write and nothing stores it:
that needs a table, a migration and a rule for when a conflict stops existing. Until then an
operator sees a conflict when they cause one and not afterwards, which is the weaker half of
what this entry asks for.
