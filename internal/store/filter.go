package store

import (
	"net/netip"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/journal"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// Page size bounds. A caller that asks for nothing gets [DefaultLimit]; one
// that asks for more than [MaxLimit] gets [MaxLimit], because a page size is
// client input and an unbounded one is a way to ask the server to allocate
// until it dies.
const (
	// DefaultLimit is the page size used when a filter names none.
	DefaultLimit = 100
	// MaxLimit is the largest page any listing returns.
	MaxLimit = 1000
)

// Cursor marks a position in a listing. Its contents are the implementation's
// business: a caller passes back what it was given and nothing else.
type Cursor string

// Page is one slice of a larger result set.
type Page[T any] struct {
	Items []T
	// NextCursor is empty once the listing is exhausted.
	NextCursor Cursor
}

// Paging is the cursor and page size shared by every listing.
type Paging struct {
	Cursor Cursor
	Limit  int
}

// EffectiveLimit returns the page size to use: [DefaultLimit] for a filter that
// names none, and never more than [MaxLimit].
func (p Paging) EffectiveLimit() int {
	if p.Limit <= 0 {
		return DefaultLimit
	}
	return min(p.Limit, MaxLimit)
}

// ZoneFilter selects zones. A zero field is not a constraint.
type ZoneFilter struct {
	Paging

	// Kind restricts the listing to forward or to reverse zones.
	Kind zone.Kind
	// Name matches one apex exactly. It is what a client resolves a name a
	// person typed into the zone it belongs to, which every command taking a
	// zone name has to do before it can do anything else. Search cannot serve
	// that: "example.com" also matches "notexample.com" and "example.com.au".
	Name zone.Name
	// Search matches anywhere in the zone name, case-insensitively. It backs
	// the instant filter above the zone list.
	Search string
	// Disabled selects only disabled or only enabled zones.
	Disabled *bool
}

// RecordFilter selects records. A zero field is not a constraint.
type RecordFilter struct {
	Paging

	ZoneID zone.ZoneID
	// Name matches one owner name exactly.
	Name zone.Name
	// Under matches an owner name at or below this name, which is how the GUI
	// expands one branch of the name tree.
	Under zone.Name
	Types []zone.RRType
	// Prefix selects the address records pointing into one network. It is how
	// a reverse zone finds the records it should be answering for.
	Prefix netip.Prefix
	// Search matches anywhere in the owner name or the record data,
	// case-insensitively.
	Search string
	// Managed selects only generated records, or only authored ones.
	Managed *bool
}

// CommitFilter selects journal commits. A zero field is not a constraint.
type CommitFilter struct {
	Paging

	ZoneID zone.ZoneID
	Kinds  []journal.Kind
	// Sources selects by what caused the change. It is what tells a change
	// somebody made from the reverse entries the server then kept in step
	// with it, which carry [journal.SourceSystem].
	Sources []journal.Source
	// Actor matches the recorded actor exactly.
	Actor string
	// Since and Until bound the commit time, Since inclusive and Until
	// exclusive.
	Since time.Time
	Until time.Time
}
