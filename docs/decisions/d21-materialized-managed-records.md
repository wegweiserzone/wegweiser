# D21 — Generated records are materialized, with provenance

- Decided: 2026-08-02

## Context

Automatic reverse management is differentiator 1. An A or AAAA record produces a PTR in the
responsible reverse zone; an RFC 2317 setup additionally produces a CNAME in the parent /24.
Those generated records have to live somewhere.

Two options:

1. **Virtual**, not stored. Synthesized when the reverse zone's snapshot is built, by
   scanning forward zones for matching addresses.
2. **Materialized**: stored as ordinary rows, with a link back to the record that caused
   them.

## Decision

Materialized. A generated PTR is a normal row in `records`, carrying `managed_by` (the ID of
the source record) and `managed_kind` (`ptr` or `rfc2317-cname`).

The applier keeps them in sync: creating an A creates the PTR in the same commit, deleting
the A deletes the PTR. The UI marks them as generated and routes edits to the source record.

## Rationale

Virtual records look elegant and are wrong for this system in four ways:

- **The journal breaks.** Invariant 4 says every change is a journal event. A virtual PTR
  never appears in the reverse zone's journal, so the reverse zone's serial does not advance
  when its contents change, and a serial that does not advance when contents change is a
  protocol violation that silently breaks IXFR and every secondary.
- **Every consumer needs a special case.** Snapshot build, zonefile export, AXFR, diff view,
  YAML export and rollback would each need to know that some records are not in the table.
  Materializing means none of them do.
- **Conflict detection gets hard.** With rows, a conflicting PTR is a uniqueness violation
  or a visible second row. Virtually, the conflict only materializes at build time, which is
  the worst moment to discover it: after the write was accepted.
- **Rollback becomes undefined.** Restoring a reverse zone to an earlier serial has no
  meaning if its contents are a function of another zone's current state.

The cost of materializing is write amplification and a synchronization obligation. Both are
bounded: one extra row per address record, kept correct by the single write path.

## Consequences

- `managed_by` is `ON DELETE CASCADE`. Deleting the source record removes its generated
  records automatically at the database level, with the applier still emitting the journal
  events so the reverse zone's serial advances correctly.
- Generated records are not directly editable through the API. An attempt returns a
  structured error naming the source record, which the GUI turns into "edit the A record
  instead" with a link. What happens when a user *insists* on overriding one is
  [D4](d04-detaching-generated-records.md).
- Enabling `auto_reverse` on a zone that already has records triggers a backfill: a single
  commit generating every missing PTR. Disabling it triggers the inverse. Both are ordinary
  commits, so both are visible in the audit log and both are revertible.
- A consistency check (`weg zone check --reverse`) verifies that managed records match what
  the rules would generate today. It exists because a rules change, an import, or a bug can
  desynchronize them, and silent desynchronization is exactly the failure the feature is
  supposed to eliminate.

## Where this stands

`weg zone check --reverse`, over `GET /zones/{zoneId}/check?reverse=true`, reports the
entries a zone's records imply and it does not have. It does not compare against a second
implementation of the rules: it plans exactly what a reconcile would write and reports that
instead of carrying it out, so the two answers cannot drift apart.

**The inverse question is deliberately not asked.** An entry that is here and would not be
generated today is not reported, because reverse automation only ever adds: an entry made
obsolete is taken away by the change that obsoleted it, and one somebody detached is theirs
to keep ([D4](d04-detaching-generated-records.md)). Asking it would need a rule for what may
legitimately remain, which D4 leaves to the person.

`weg zone reconcile` and `POST /zones/{zoneId}/reconcile` carry out what the check reports.
Until they existed, `Applier.Reconcile` had been written and tested and was called by
nothing, so the backfill this record describes never ran.

**The two triggers this record names are still not wired.** Creating a reverse zone for a
network already in use does not fill it, and enabling `auto_reverse` on a zone that already
has records does not backfill; both are the explicit command for now. Disabling
`auto_reverse` triggering the inverse is further off than that, because reconcile only adds
and what may be removed is the question D4 left open.
