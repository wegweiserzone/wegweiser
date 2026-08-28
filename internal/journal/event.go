package journal

import (
	"fmt"
	"strings"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// Op is the direction of a single resource-record change.
//
// Two are enough. A modification is a deletion followed by an addition, which
// is also exactly how incremental zone transfer expresses it (RFC 1995 §2), so
// a third operation would only add a case every consumer has to unfold again.
type Op string

const (
	// OpDel removes a resource record.
	OpDel Op = "del"
	// OpAdd adds a resource record.
	OpAdd Op = "add"
)

// Valid reports whether o is one of the defined operations.
func (o Op) Valid() bool { return o == OpAdd || o == OpDel }

// Event is one resource-record change inside a [Commit].
//
// An event carries the record's full data in both directions, deletions
// included. RFC 1995 §2 lists deleted records in full rather than by name, and
// a rollback that has to put a record back needs the same thing.
//
// Unlike a [zone.Record] an event may carry an SOA. The SOA is zone metadata
// rather than a record (data model §4.1), but a change to its
// parameters still travels through the journal, because that is where the
// history of the zone lives.
type Event struct {
	// Seq orders events within a commit, counting from zero.
	Seq int

	Op Op

	Name  zone.Name
	Class zone.Class
	Type  zone.RRType
	TTL   zone.TTL
	RData zone.RData
}

// Validate reports whether the event describes a well-formed change.
func (e Event) Validate() error {
	if !e.Op.Valid() {
		return fmt.Errorf("%w journal operation %q", zone.ErrInvalid, e.Op)
	}
	if e.Seq < 0 {
		return fmt.Errorf("%w: event sequence number %d is negative", zone.ErrInvalid, e.Seq)
	}
	if e.Name.IsZero() {
		return fmt.Errorf("%w: an event needs an owner name", zone.ErrInvalid)
	}
	if !e.Class.Storable() {
		return fmt.Errorf("%w: class %s exists only inside a message, not in a zone (RFC 6895 §3.2)",
			zone.ErrInvalid, e.Class)
	}
	if !e.Type.Storable() {
		return fmt.Errorf("%w: %s exists only inside a message, not in a zone (RFC 6895 §3.1)",
			zone.ErrInvalid, e.Type)
	}
	if !e.TTL.Valid() {
		return fmt.Errorf("%w: TTL of %d exceeds the maximum of %d (RFC 2181 §8)",
			zone.ErrInvalid, e.TTL, zone.MaxTTL)
	}
	if e.RData.IsZero() {
		return fmt.Errorf("%w: an event needs the record data, so that a %s can be undone as well as replayed",
			zone.ErrInvalid, e.Op)
	}
	return nil
}

// String renders the event as one diff line: the record in zonefile order,
// prefixed by "-" for a deletion and "+" for an addition.
func (e Event) String() string {
	var b strings.Builder
	if e.Op == OpDel {
		b.WriteByte('-')
	} else {
		b.WriteByte('+')
	}
	b.WriteString(e.Name.String())
	b.WriteByte('\t')
	b.WriteString(e.TTL.String())
	b.WriteByte('\t')
	b.WriteString(e.Class.String())
	b.WriteByte('\t')
	b.WriteString(e.Type.String())
	b.WriteByte('\t')
	b.WriteString(e.RData.String())
	return b.String()
}
