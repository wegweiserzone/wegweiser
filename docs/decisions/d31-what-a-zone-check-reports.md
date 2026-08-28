# D31 — A zone check reports refusals and diagnoses, and says which is which

- Decided: 2026-08-28

## Context

`weg zone check` applies the rules the write path enforces to a zone as it is stored, and
lists what fails. Every finding it has today is the same kind of thing: something this server
would refuse to write, so a zone holding one holds data nothing will ever answer.

Two more things want to be in that report and are not that kind of thing.

**A lame delegation.** An NS record pointing at a name inside the zone it delegates to needs
an address record beside it, or a resolver following the delegation has a circular dependency
it cannot escape (RFC 1912 §2.8). This is legal DNS. The write path accepts it and would
accept it again, because the missing record may simply not have been written yet.

It is also already implemented twice, client-side: `lameNameServers` in
`internal/cli/zone_health.go` and `lameNameServers` in `web/src/lib/health.ts`, kept in step
by hand. The comment at the top of the second one set the condition for changing that: if a
second rule ever joined it, the rules belonged on the server rather than in both clients. A
server-side check is that second rule.

**Reverse drift.** [D21](d21-materialized-managed-records.md) asks for a check that generated
records still match what the rules would generate today. A record that no longer matches is
not invalid either. It is what an older rule, or a detach (D4), or a bug left behind.

Putting these in one undifferentiated list would say that a zone with a missing glue record
is in the same condition as a zone holding a record nothing can answer. It is not.

## Decision

**A finding carries whether the write path would refuse it.** Two values, and the words are
the ones every reader already has:

- **error.** The write path refuses this. A zone holding one holds data that is unanswerable
  or contradictory, and this server would not have let anybody build it. Something reached the
  database another way.
- **warning.** The write path accepts this and would again. It is a diagnosis: correct DNS
  that is probably not what somebody meant. It may be exactly what they meant.

The distinction is not a severity scale and does not grow a third value. It is one question
with a yes and a no: would the server have refused it? Anything that needs finer grading is
asking for a different report.

**A diagnosis is computed on the server.** Not in each client. `internal/cli/zone_health.go`
and `web/src/lib/health.ts` are deleted, and both clients read the answer instead of
deriving it. Two implementations of one rule in two languages is a rule that will drift, and
the only reason it did not was that there was one of them.

**A cheap diagnosis is still shown without being asked for.** The lame delegation appears
beside the zone in `weg zone show` and on the zone screen today, and it stays there. Somebody
who has to know to run a check in order to be told their delegation is broken has been told
nothing: this product exists so that a person does not build something that quietly does not
work. What makes that affordable is that the answer needs the zone's NS records and one
lookup per distinct target, not a walk of the zone.

## Consequences

`zone.Finding` gains the field, and every finding that exists today is an error. The check
gains a second phase for what a pure walk of the records cannot answer on its own: whether a
name inside the zone has an address is a question for the store.

The exit status of `weg zone check` stays zero, and this is what would make changing it
defensible later. "Errors, and no warnings" is a condition a script can be given; "findings"
is not, because it is two things.

A warning is not a thing to be cleared. A zone can carry one for a reason, and nothing here
gains a way to acknowledge or suppress one. If that turns out to be needed, it is a record of
its own, because a suppression list is state and state belongs somewhere.

This record assumes [D29](d29-a-node-that-cannot-apply.md) and
[D30](d30-what-a-log-snapshot-contains.md) take the numbers they are drafted under. If either
is dropped, this one is renumbered rather than leaving a gap.
