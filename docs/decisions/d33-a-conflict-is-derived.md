# D33 — A conflict is derived, not stored

- Decided: 2026-08-29
- Amends: [D3](d03-ptr-conflicts.md)

## Context

D3 made a PTR conflict a first-class object: returned in the API response, listed under the
zone, and clearable. Only the first is built. The other two were left because a conflict is
worked out while a write happens and nothing keeps it, so listing it needs a table, a
migration, and a rule for when a conflict stops existing.

That last one is where it stalled, and it stalled because the question is the wrong one. A
conflict is not something that happened. It is a state two records are in: two names point
at one address, and only one of them has the reverse entry. It stops holding when that stops
being true, and the ways that can happen are all writes somewhere else. Either record can
go. The operator can make the other name the canonical one. The policy can become `multi`,
at which point both entries are wanted and there was never anything to report. A stored row
would have to be kept in step with every one of those, and each is a chance for the row and
the records to disagree.

Nothing has to be computed specially either. `Applier.PlanReconcile` already works out, for
a whole zone and without writing anything, what reverse automation would generate and what
it would refuse. The refusals are its `Result.Conflicts`, each carrying the source record,
the address, where the entry would have gone, and the name holding it instead. That is the
listing D3 asked for, thrown away today by the one caller that asks for the plan.

## Decision

**A conflict is derived from the records, never stored.** No table, no migration, and no
rule for when one stops existing: it stops when the records stop being in that state, which
is the only definition that cannot drift from them.

**The zone check is where they are listed.** `weg zone check --reverse` plans the same
reconcile and reports what is missing; a conflict is the other half of that answer, and it
costs nothing that is not already being spent. It is a warning by [D31](d31-what-a-zone-check-reports.md):
the write path accepted it and would again, and under first-wins it may be exactly what
somebody meant.

**"Clearable" was the wrong word and is dropped.** Nothing gains a way to dismiss a conflict
that still holds, because dismissing one would mean hiding a state the records are still in.
What D3 wanted from it is the action beside it: make this name the canonical one. That
changes the records, and the conflict is then not true rather than not shown.

## Consequences

D3's obligation is met by the check rather than by a table, and its "listed under the zone"
reads as "listed by the zone's check".

**The one-click action is still owed.** D3 asks for "make this the canonical name" and
neither client has it. It is an ordinary edit, it needs the conflict's own fields to build
the change, and there is now a screen with somewhere to put it: the check already offers an
action for the reverse findings beside these.

Listing conflicts costs a planning transaction, which is why it arrives behind the same flag
the missing entries do rather than on every check.
