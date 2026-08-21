# ADR 0003 — Store individual records, enforce RRsets in validation

- Status: proposed
- Date: 2026-08-02

## Context

DNS does not operate on records, it operates on RRsets: all records sharing owner name,
class and type (RFC 2181 §5). An RRset is answered as a unit, cached as a unit, and its
members must share one TTL (RFC 2181 §5.2).

A schema can mirror that (one row per RRset, RDATA as a list) which makes the constraints
structural and unbreakable. Or it can store one row per record and enforce the constraints
in code.

## Decision

One row per record. RRset rules are enforced in `internal/zone` validation, backed by a
database uniqueness index for duplicate RRs, and re-checkable via `weg zone check`.

RRsets are reassembled when the snapshot is built and when a zone is exported. The query
path only ever sees RRsets, never individual records.

## Rationale

Every UI affordance the product promises needs per-record identity:

- a **per-record comment** ("this A is the backup MX host"): the single most requested
  feature missing from most DNS admin tools
- **provenance for generated records**: a PTR points back to the exact A record that caused
  it (see ADR 0004); an RRset-level link cannot express which member caused what
- a **stable ID** for a diff line to anchor to, and for the API to address in a `PATCH`
- **row selection** in a virtualized table, where the user selects three of five A records

An RRset row would force a synthetic sub-identity for each member: an index into a list,
which is unstable the moment a member is removed. That is worse than the constraint it buys.

The constraints being enforced in code rather than by the schema is acceptable because they
are enforced in exactly one place (the applier is the only write path (invariant 4)) and
because the expensive one to get wrong, duplicate RRs, is still a database index
(`records_rr_uq`).

## Consequences

- Uniform TTL within an RRset is a validation rule, not a constraint. Adding a record with a
  divergent TTL is rejected; changing a TTL updates the whole RRset. Both need explicit
  tests, since a bug here is invisible until a resolver caches inconsistently.
- `weg zone check` and a startup consistency check exist to catch state written by an older
  buggy version or by a hand-edited database.
- The snapshot builder groups records into RRsets while streaming in canonical order. Since
  `records_zone_sort_idx` already delivers `(sort_key, rrtype)` order, grouping is a single
  pass with no sorting and no map.
- The API exposes both granularities: record-level operations for editing, and an RRset
  replace operation, because "set the A records for www to exactly these three" is the
  operation a GitOps import wants and cannot express as a sequence of adds and deletes
  without a read-modify-write race.
