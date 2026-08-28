# D2 — SOA serials increment by one, always

The zone serial is an internal counter. One commit advances it by exactly one, using
RFC 1982 arithmetic. There is no unix-time policy and no `YYYYMMDDnn` date counter.

The date counter is the BIND convention and it is genuinely worse: it caps a zone at 99
changes per day, which any bulk import exceeds, and it breaks the one-commit-one-step
invariant that lets IXFR replay the journal directly instead of reconstructing differences.

**Obligation: imports seed, they do not reset.** When a zone is imported from a zonefile or
an AXFR, the incoming SOA serial becomes the starting value and counting continues from
there. Starting a migrated zone at 1 would make every existing secondary consider our data
older than what it already has (RFC 1982 §3.2) and refuse to transfer it. This is the single
most common way to break a migration, so it gets a test.

**Obligation: serial comparison never uses `<`.** A `serial.Compare(a, b)` helper in
`internal/journal` is the only permitted comparison, and using `<` on a serial is a lint
target.

## Where this stands

Both obligations hold, and the second is met by something stronger than what is asked for
above.

Seeding is built, with the test this record names: `TestImportSeedsTheSerial` in
`internal/apply`.

The comparison is not a `serial.Compare(a, b)` helper in `internal/journal`. It is
`Serial.Compare`, a method on `zone.Serial` in `internal/zone`. There is no lint rule
against `<`, and none is needed: `Serial` is a struct around an unexported field, so `<`
does not compile. What this record wanted a linter for, the type system refuses outright.
