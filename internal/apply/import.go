package apply

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// Import is a whole zone arriving at once.
//
// It is deliberately not a zonefile. A file is one way a zone arrives and a
// declarative configuration is another, and neither belongs in the write path:
// zonefiles are an import and export format, not a storage format
// (architecture invariant 5). What arrives here is a name, the zone's own
// settings and its records.
type Import struct {
	// Name is the zone apex.
	Name zone.Name

	// SOA is the zone's own settings, serial included. The serial is the one
	// the zone had wherever it came from, and it is kept: starting a migrated
	// zone at 1 would make every existing secondary consider our copy older
	// than what it already has (RFC 1982 §3.2) and refuse to transfer it. See
	// docs/decisions.md D2.
	SOA zone.SOA

	// Records is everything the zone holds, apart from the SOA.
	Records []zone.Record

	// DefaultTTL is what a record added later gets when it names no TTL. Zero
	// takes the SOA's own TTL, which for a file is what its $TTL directive
	// most likely said.
	DefaultTTL zone.TTL
}

// Skipped is a record an import did not write, and the reason.
//
// It travels as data rather than as an error, for the reason docs/decisions.md
// D3 and D6 give about conflicts and missing reverse zones: this is something a
// person has to decide about, and refusing the whole file over it would mean
// nobody can migrate a zone that has an oddity in it.
type Skipped struct {
	Record zone.Record
	Reason string
}

// Import brings a whole zone in at once.
//
// The zone must not already exist. A file is the complete contents of a zone,
// so importing into a zone that already holds records is a replacement rather
// than an import, and doing that silently would be the difference between
// gaining a zone and losing one.
func (a *Applier) Import(ctx context.Context, in Import, meta Meta) (*Result, error) {
	if err := meta.Validate(); err != nil {
		return nil, err
	}
	if in.Name.IsZero() {
		return nil, fmt.Errorf("%w: an import names the zone it brings in", zone.ErrInvalid)
	}
	if err := in.SOA.Validate(); err != nil {
		return nil, fmt.Errorf("the start of authority of %q: %w", in.Name, err)
	}

	existing, err := a.zoneNamed(ctx, in.Name)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, fmt.Errorf(
			"%w: the zone %q already exists; an import is the complete contents of a zone, so "+
				"bringing this one in would replace what is there rather than add to it",
			store.ErrConflict, in.Name)
	}

	// Built through the constructor rather than field by field: it is what
	// works out whether this is a reverse zone and, if it is, the network it
	// answers for. An in-addr.arpa file assembled by hand here would arrive as
	// a forward zone and none of the reverse automation would ever look at it.
	z, err := zone.NewZone(in.Name, in.SOA)
	if err != nil {
		return nil, err
	}
	z.DefaultTTL = in.DefaultTTL
	if z.DefaultTTL == 0 {
		z.DefaultTTL = in.SOA.TTL
	}

	records, skipped := keepAnswerable(in.Name, in.Records)

	res, err := a.CreateZone(ctx, &z, records, meta)
	if err != nil {
		return nil, err
	}
	res.Skipped = skipped
	return res, nil
}

// zoneNamed returns the zone with that apex, or nil when there is none.
func (a *Applier) zoneNamed(ctx context.Context, name zone.Name) (*zone.Zone, error) {
	var z *zone.Zone
	err := a.store.View(ctx, func(r store.Reader) error {
		var verr error
		z, verr = r.ZoneByName(ctx, name)
		if errors.Is(verr, store.ErrNotFound) {
			z, verr = nil, nil
		}
		return verr
	})
	return z, err
}

// keepAnswerable splits the records into the ones the zone can answer with and
// the ones it never could.
//
// The rule is the one every other write goes through (at a delegation only NS,
// below one only glue (RFC 1034 §4.2.1)) applied here against the incoming
// records rather than against the database, because an import is the whole zone
// and therefore knows all of its own delegations without asking.
func keepAnswerable(apex zone.Name, records []zone.Record) (keep []zone.Record, skipped []Skipped) {
	delegations := make(map[zone.Name]struct{})
	byOwner := make(map[zone.Name][]zone.Record, len(records))
	var owners []zone.Name

	for i := range records {
		r := records[i]
		if _, seen := byOwner[r.Name]; !seen {
			owners = append(owners, r.Name)
		}
		byOwner[r.Name] = append(byOwner[r.Name], r)
		if r.Type == zone.TypeNS && !r.Name.Equal(apex) {
			delegations[r.Name] = struct{}{}
		}
	}

	// Sorted, so that a file which is imported twice reports the same records
	// in the same order both times.
	slices.SortFunc(owners, func(a, b zone.Name) int { return a.Compare(b) })

	keep = make([]zone.Record, 0, len(records))
	for _, owner := range owners {
		owned := byOwner[owner]
		point, ok := closestIn(owner, delegations)
		if !ok {
			keep = append(keep, owned...)
			continue
		}
		if err := zone.ValidateUnderDelegation(owner, owned, point); err == nil {
			keep = append(keep, owned...)
			continue
		}
		// The whole owner name goes, because the rule is about what may live
		// at a name rather than about one record: below a delegation an A may
		// stay as glue where a TXT beside it may not, so each is judged on its
		// own type.
		for i := range owned {
			r := owned[i]
			if err := zone.ValidateUnderDelegation(owner, owned[i:i+1], point); err == nil {
				keep = append(keep, r)
				continue
			}
			skipped = append(skipped, Skipped{
				Record: r,
				Reason: fmt.Sprintf(
					"%q lies at or below the delegation at %q, where a query is referred to the "+
						"child and a %s record would never be answered (RFC 1034 §4.2.1)",
					r.Name, point, r.Type),
			})
		}
	}
	return keep, skipped
}

// closestIn returns the nearest delegation at or above name.
func closestIn(name zone.Name, delegations map[zone.Name]struct{}) (zone.Name, bool) {
	for n := name; ; {
		if _, ok := delegations[n]; ok {
			return n, true
		}
		parent, ok := n.Parent()
		if !ok {
			return zone.Name{}, false
		}
		n = parent
	}
}
