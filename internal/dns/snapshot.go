package dns

import (
	"context"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// Snapshot is every zone the server answers from, frozen.
//
// The zero value and a nil *Snapshot are both an empty snapshot that answers
// for nothing, so a server can be wired up before its first build without a nil
// check on the query path.
type Snapshot struct {
	// zones is keyed by apex name, and finding the zone for a query name is the
	// same walk as finding a name inside a zone. Zone count is in the thousands
	// where record count is in the millions, which is why this is a structure
	// of its own rather than the top of one big tree of names: replacing a zone
	// then touches this map and nothing else.
	zones map[zone.Name]*zoneTree

	// records is the total across zones, kept as a running count so a metric
	// does not have to walk every zone to report it.
	records int

	// depth is the label count of the deepest apex. A query name longer than
	// that is cut back before the walk starts.
	depth int
}

// Build assembles a snapshot from the given zones and their stored records.
//
// The build is sequential. At the record counts of D12 the cost is dominated by
// parsing record data, not by waiting on the store, and a parallel build is a
// contained change here if the cold-start budget ever demands it.
func Build(ctx context.Context, zones []*zone.Zone, src RecordSource) (*Snapshot, error) {
	s := &Snapshot{zones: make(map[zone.Name]*zoneTree, len(zones))}
	for _, z := range zones {
		if z.Disabled {
			continue
		}
		t, err := buildZone(ctx, z, src)
		if err != nil {
			return nil, err
		}
		s.zones[z.Name] = t
		s.records += t.count
		s.depth = max(s.depth, z.Name.LabelCount())
	}
	return s, nil
}

// WithZone returns a snapshot in which one zone has been rebuilt from the
// store, sharing every other zone with the receiver.
//
// This is what a commit publishes. The new zone is built completely before the
// result is returned, so a caller that swaps it in exposes either the whole old
// state or the whole new one, never a half-built zone. A zone that has been
// disabled is removed rather than rebuilt.
func (s *Snapshot) WithZone(ctx context.Context, z *zone.Zone, src RecordSource) (*Snapshot, error) {
	if z.Disabled {
		return s.WithoutZone(z.Name), nil
	}
	t, err := buildZone(ctx, z, src)
	if err != nil {
		return nil, err
	}
	next := s.clone(z.Name, t)
	return next, nil
}

// WithoutZone returns a snapshot with one zone gone, sharing the rest with the
// receiver. It returns the receiver unchanged when the zone was not there.
func (s *Snapshot) WithoutZone(name zone.Name) *Snapshot {
	if s.zoneAt(name) == nil {
		return s
	}
	return s.clone(name, nil)
}

// Zones returns the number of zones the snapshot answers for.
func (s *Snapshot) Zones() int {
	if s == nil {
		return 0
	}
	return len(s.zones)
}

// Records returns the number of records the snapshot answers from, counting the
// SOA each zone carries.
func (s *Snapshot) Records() int {
	if s == nil {
		return 0
	}
	return s.records
}

// clone returns a copy of the snapshot with one apex replaced, added, or, for
// a nil tree, removed. Every other zone is carried over by pointer.
//
// Copying the map is O(zones) against a commit budget of 200 ms (D12), and at
// the thousands of zones this is sized for it costs a fraction of the zone
// rebuild it accompanies. The counts are worked out from what is copied rather
// than adjusted, so they cannot drift from the map.
func (s *Snapshot) clone(name zone.Name, t *zoneTree) *Snapshot {
	size := 1
	if s != nil {
		size = len(s.zones) + 1
	}
	next := &Snapshot{zones: make(map[zone.Name]*zoneTree, size)}

	if s != nil {
		for zoneName, tree := range s.zones {
			if zoneName.Equal(name) {
				continue
			}
			next.add(zoneName, tree)
		}
	}
	if t != nil {
		next.add(name, t)
	}
	return next
}

// add puts a zone in the map and keeps the running totals with it.
func (s *Snapshot) add(name zone.Name, t *zoneTree) {
	s.zones[name] = t
	s.records += t.count
	s.depth = max(s.depth, name.LabelCount())
}

// zoneFor returns the most specific zone qname falls inside: the zone whose
// apex is the longest suffix of qname. It returns nil when the server holds no
// zone for the name, which is a query to refuse rather than to answer: a
// server cannot assert that a name does not exist in a namespace it does not
// serve.
func (s *Snapshot) zoneFor(qname zone.Name) *zoneTree {
	if s == nil {
		return nil
	}

	name := qname
	for depth := name.LabelCount(); depth > s.depth; depth-- {
		parent, ok := name.Parent()
		if !ok {
			break
		}
		name = parent
	}

	for cur := name; ; {
		if t := s.zones[cur]; t != nil {
			return t
		}
		parent, ok := cur.Parent()
		if !ok {
			return nil
		}
		cur = parent
	}
}

// zoneAt returns the zone whose apex is exactly name, or nil.
func (s *Snapshot) zoneAt(name zone.Name) *zoneTree {
	if s == nil {
		return nil
	}
	return s.zones[name]
}
