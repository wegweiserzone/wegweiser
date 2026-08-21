// Package dns is the data plane: it answers queries and does nothing else.
//
// Everything here reads from an immutable [Snapshot]. Nothing here touches the
// database. The control plane builds a snapshot from stored records and swaps
// it in with a single atomic store, so a zone change never blocks a query that
// is already in flight, and a query holds one consistent view of the world for
// its whole lifetime without taking a lock. That is architecture invariant 2,
// and it is enforced mechanically: depguard refuses an import of
// internal/store from this package.
//
// The store is the source of truth and the snapshot is a cache derived from it
// (invariant 8). A snapshot can always be rebuilt from the database; the
// database is never rebuilt from a snapshot. Crash recovery is therefore the
// same code path as startup, and there is nothing here to repair.
package dns
