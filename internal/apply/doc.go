// Package apply is the write path, and the only thing that changes zone data.
//
// A client submits a [Command]. The applier works out what it amounts to
// against the zone as it stands, writes it, and records it as one
// [journal.Commit] in the same transaction. Nothing else advances a zone
// serial, and no record is written without a commit explaining it (invariant 4).
//
// Three cases are returned to the caller rather than logged:
//
//   - the address already answers with another name (D3). The existing entry
//     stays and [Conflict] reports it.
//   - no zone exists to hold the entry (D6). [MissingZone] names the zone that
//     would be needed, since creating one asserts authority over a namespace.
//   - a different entry is wanted (D4). Editing a generated record is refused;
//     [ActionDetach] hands it over instead.
//
// RFC 2317 is handled on both sides. An entry for an address inside a classless
// child goes under that child's apex, and where this server also holds the
// parent, the CNAME pointing there is written too (D7). Provenance is a chain,
// delegation to entry to address record, and removals are ordered from the
// links rather than assumed.
//
// A reverse zone created for a network already in use has no change to react
// to. [Applier.Reconcile] fills it, and moves what the arrival of a more
// specific zone has overtaken. It only adds: obsolete entries were removed by
// the change that obsoleted them, and a detached entry (D4) is not the
// automation's to touch.
package apply
