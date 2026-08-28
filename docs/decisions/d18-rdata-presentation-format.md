# D18 — Store RDATA in canonical presentation format

- Decided: 2026-08-01

## Context

Every record needs its RDATA persisted. Three representations were considered:

1. **Wire format** (binary blob): what goes on the network.
2. **Typed columns per RR type**: `a_address`, `mx_pref`, `mx_target`, …
3. **Presentation format** (text): what appears in a zonefile.

The choice constrains diffing, search, import/export, unknown-type support and query-path
cost.

## Decision

RDATA is stored as **canonical presentation format text**: fully qualified, origin
independent, lowercase for domain names inside RDATA, normalized by `miekg/dns` on write.

`www.example.com. MX 10 mail.example.com.` stores `10 mail.example.com.`, never `10 mail`.

Unknown RR types use the RFC 3597 `\# <length> <hex>` form, which round-trips losslessly.

Two derived columns exist alongside it: `rdata_hash` for the uniqueness index (TXT RDATA can
reach 64 KB, which cannot go in a B-tree key) and `addr` for A/AAAA, indexed so reverse
automation can find names by address without a scan.

## Rationale

The product is built on diffs, audit logs and human editing. Presentation format is the only
representation where a journal event is readable as-is, a diff view needs no rendering step,
a `LIKE` search over RDATA works, and a zonefile export is a concatenation rather than a
conversion.

Wire format wins on compactness and exactness but makes every one of those a decode step,
and puts binary blobs in the audit log that no operator can read. Typed columns are the
worst of both: a schema migration per new RR type, and RFC 3597 unknown types become
unrepresentable, which is a spec violation, not a missing feature.

The usual objection to presentation format is parsing cost. It does not apply here: parsing
happens only when a snapshot is built, never on the query path, which serves pre-parsed
`dns.RR` values from memory.

Storing names inside RDATA fully qualified removes the `$ORIGIN` ambiguity that makes
zonefiles fragile: a record's meaning never depends on where it appears in a file.

## Consequences

- Import must canonicalize: relative names expanded, casing normalized, whitespace collapsed.
  Two spellings of the same RDATA must produce identical bytes, otherwise `records_rr_uq`
  fails to catch duplicates.
- A canonicalization round-trip test (`parse → store → render → parse`) is required across
  all supported types, plus a fuzz target.
- Comparing RDATA for equality is a string comparison. That is only correct because of the
  canonicalization above: the round-trip test guards it.
- Storage is larger than wire format. At ten million records this is on the order of
  hundreds of megabytes on disk and zero in RAM, since the query path holds parsed records.
