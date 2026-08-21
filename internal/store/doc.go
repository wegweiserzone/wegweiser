// Package store defines the persistence boundary of Wegweiser.
//
// It is the only place that knows SQL exists (architecture invariant 3).
// Everything above it works in terms of [zone.Zone], [zone.Record] and
// [journal.Commit], and the boundary is enforced mechanically: depguard refuses
// an import of database/sql or of a driver anywhere outside this package tree.
//
// Implementations live in subpackages: sqlite today, postgres later. Callers
// hold the interface, never the implementation, so a backend swap is a wiring
// change in cmd/weg and nothing else. Where backends genuinely differ, the
// difference is reported by [Capabilities] rather than discovered by a type
// assertion.
//
// The store is the source of truth. The in-memory snapshot the query path
// serves from is a derived cache that can always be rebuilt from here, and
// never the other way around (architecture invariant 8).
package store
