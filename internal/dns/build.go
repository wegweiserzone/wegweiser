package dns

import (
	"context"
	"fmt"
	"iter"
	"strings"

	wire "github.com/miekg/dns"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// Source is everything a snapshot is built from: the pair of streams a rebuild
// reads, which a store satisfies by being what it is. An interface rather than
// a store handle because the query path may not import the store (invariant 2).
type Source interface {
	RecordSource

	// IterZones streams every zone, in any order.
	IterZones(ctx context.Context) iter.Seq2[*zone.Zone, error]
}

// Rebuild builds a snapshot from everything src holds.
//
// It reads, patches nothing, and carries nothing over from an earlier snapshot
// (invariant 8), so startup and crash recovery are the same path and neither
// needs repair logic.
func Rebuild(ctx context.Context, src Source) (*Snapshot, error) {
	var zones []*zone.Zone
	for z, err := range src.IterZones(ctx) {
		if err != nil {
			return nil, fmt.Errorf("read the zones to build a snapshot from: %w", err)
		}
		zones = append(zones, z)
	}
	return Build(ctx, zones, src)
}

// RecordSource yields the stored records of one zone, in any order. An
// interface rather than a store handle because the query path may not import
// the store (invariant 2); the method name matches store.Reader, so no adapter
// is needed.
type RecordSource interface {
	IterZoneRecords(ctx context.Context, id zone.ZoneID) iter.Seq2[*zone.Record, error]
}

// buildZone reads a zone out of the store and returns it in answerable form. A
// build is a pure function of what the store holds, which is what makes a
// rebuild after a crash indistinguishable from one after a commit.
func buildZone(ctx context.Context, z *zone.Zone, src RecordSource) (*zoneTree, error) {
	t := &zoneTree{
		name:   z.Name,
		soa:    z.SOA,
		negTTL: z.SOA.NegativeTTL(),
		nodes:  make(map[zone.Name]*node, 8),
	}

	// The SOA is a field of the zone rather than a record in the table (the
	// serial belongs to the journal, not to whoever last edited the zone) so
	// the one the wire needs is made here. See data model §4.1.
	soa, err := soaRR(z, z.SOA.TTL)
	if err != nil {
		return nil, err
	}
	t.node(z.Name).add(zone.ClassIN, zone.TypeSOA, soa)
	t.count++

	// Negative answers carry the same SOA with the shorter TTL of RFC 2308 §3,
	// so the second one is built here and the query path never has to touch a
	// record it shares with every other query.
	if t.negSOA, err = soaRR(z, t.negTTL); err != nil {
		return nil, err
	}

	var delegations map[zone.Name]*node

	for rec, err := range src.IterZoneRecords(ctx, z.ID) {
		if err != nil {
			return nil, fmt.Errorf("read the records of %q: %w", z.Name, err)
		}
		if rec.Disabled {
			continue
		}
		// A stored SOA would be a second answer to a question the zone already
		// answers, and the two could disagree about the serial.
		if rec.Type == zone.TypeSOA {
			continue
		}
		if !z.Contains(rec.Name) {
			return nil, fmt.Errorf("record %s at %q lies outside the zone %q",
				rec.ID, rec.Name, z.Name)
		}

		rr, err := recordRR(rec)
		if err != nil {
			return nil, err
		}
		n := t.node(rec.Name)
		set := n.add(rec.Class, rec.Type, rr)
		t.count++

		// The query path needs some targets as names rather than as text, so
		// they are parsed here rather than on every query that follows one.
		if pointsAtAName(rec.Type) {
			target, err := targetName(rr)
			if err != nil {
				return nil, fmt.Errorf("record %s (%s): %w", rec.ID, rec, err)
			}
			set.targets = append(set.targets, target)
		}

		// NS below the apex is a delegation. NS at the apex is the zone naming
		// its own servers (RFC 1034 §4.2.1), which is ordinary data.
		if rec.Type == zone.TypeNS && !z.IsApex(rec.Name) {
			if delegations == nil {
				delegations = make(map[zone.Name]*node, 4)
			}
			delegations[rec.Name] = n
			n.delegation = n
		}
	}

	t.link(delegations)
	return t, nil
}

// recordRR renders a stored record in the form the wire library packs, through
// the presentation format that D18 makes canonical: the text came out of
// this same library on the way in, so it parses back to what it was. One parse
// per record per rebuild dominates a build, and is the first place to look if
// the cold-start budget of D12 is missed.
func recordRR(rec *zone.Record) (wire.RR, error) {
	rr, err := wire.NewRR(rec.String())
	if err != nil {
		return nil, fmt.Errorf("record %s (%s): %w", rec.ID, rec, err)
	}
	if rr == nil {
		return nil, fmt.Errorf("record %s (%s): parsed to nothing", rec.ID, rec)
	}
	return rr, nil
}

// pointsAtAName reports whether the query path needs a record's target ahead of
// time: the CNAME it chases (RFC 1034 §4.3.2 step 3a) and the NS, MX and SRV
// whose addresses fill the additional section (step 6). Parsing any other type
// would cost memory per record for no answer.
func pointsAtAName(t zone.RRType) bool {
	switch t {
	case zone.TypeCNAME, zone.TypeNS, zone.TypeMX, zone.TypeSRV:
		return true
	default:
		return false
	}
}

// targetName returns the name a record points at, in the form the query path
// follows it with. It handles exactly the types [pointsAtAName] admits and
// refuses the rest rather than returning a zero name, so a missing case fails
// the rebuild instead of producing a snapshot that answers wrongly.
func targetName(rr wire.RR) (zone.Name, error) {
	switch r := rr.(type) {
	case *wire.CNAME:
		return zone.ParseName(r.Target)
	case *wire.NS:
		return zone.ParseName(r.Ns)
	case *wire.MX:
		return zone.ParseName(r.Mx)
	case *wire.SRV:
		return zone.ParseName(r.Target)
	default:
		return zone.Name{}, fmt.Errorf("%w: a %T carries no target name", zone.ErrInvalid, rr)
	}
}

// soaRR builds the apex SOA from the zone's own parameters, with the given TTL.
//
// The TTL is a parameter because a zone needs the record twice: once as it is
// answered, and once at the shorter negative-caching TTL of RFC 2308 §3.
func soaRR(z *zone.Zone, ttl zone.TTL) (wire.RR, error) {
	return soaRRFor(z.Name, ttl, z.SOA)
}

// soaRRFor builds a start of authority from parameters that need not be the
// ones a zone currently holds, which is what an incremental transfer sends: a
// difference sequence is framed by the version it came from as well as the one
// it reaches (RFC 1995 §4).
func soaRRFor(name zone.Name, ttl zone.TTL, soa zone.SOA) (wire.RR, error) {
	var b strings.Builder
	b.WriteString(name.String())
	b.WriteByte('\t')
	b.WriteString(ttl.String())
	b.WriteString("\tIN\tSOA\t")
	b.WriteString(soa.RData())

	rr, err := wire.NewRR(b.String())
	if err != nil {
		return nil, fmt.Errorf("SOA of %q (%s): %w", name, b.String(), err)
	}
	return rr, nil
}
