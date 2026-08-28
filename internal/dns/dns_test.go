package dns

import (
	"context"
	"iter"
	"strings"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/id"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// source is a stand-in for the store: it hands out the records it was given and
// can be told to fail partway through, which is the case a real store produces
// when a read dies mid-stream.
type source struct {
	zones   []*zone.Zone
	records map[zone.ZoneID][]*zone.Record
	err     error

	// zoneErr fails the zone stream rather than the record stream, which is a
	// different moment in a rebuild and a different error path.
	zoneErr error
}

func (s source) IterZones(_ context.Context) iter.Seq2[*zone.Zone, error] {
	return func(yield func(*zone.Zone, error) bool) {
		for _, z := range s.zones {
			if !yield(z, nil) {
				return
			}
		}
		if s.zoneErr != nil {
			yield(nil, s.zoneErr)
		}
	}
}

func (s source) IterZoneRecords(_ context.Context, zid zone.ZoneID) iter.Seq2[*zone.Record, error] {
	return func(yield func(*zone.Record, error) bool) {
		for _, r := range s.records[zid] {
			if !yield(r, nil) {
				return
			}
		}
		if s.err != nil {
			yield(nil, s.err)
		}
	}
}

// newZone builds a zone with the usual defaults and an identity of its own.
func newZone(t testing.TB, name string) *zone.Zone {
	t.Helper()
	// Names below the root gain a label; below the root itself they do not.
	under := name
	if name == "." {
		under = ""
	}
	z, err := zone.NewZone(
		zone.MustParseName(name),
		zone.DefaultSOA(zone.MustParseName("ns1."+under), zone.MustParseName("hostmaster."+under)),
	)
	if err != nil {
		t.Fatalf("new zone %q: %v", name, err)
	}
	z.ID = zone.ZoneID(id.New())
	return &z
}

// record parses one zonefile line into a stored record of the given zone.
func record(t testing.TB, zid zone.ZoneID, line string) *zone.Record {
	t.Helper()
	fields := strings.SplitN(line, " ", 5)
	if len(fields) != 5 {
		t.Fatalf("record %q: want 5 fields, got %d", line, len(fields))
	}

	ttl, err := zone.ParseTTL(fields[1])
	if err != nil {
		t.Fatalf("record %q: %v", line, err)
	}
	class, err := zone.ParseClass(fields[2])
	if err != nil {
		t.Fatalf("record %q: %v", line, err)
	}
	typ, err := zone.ParseRRType(fields[3])
	if err != nil {
		t.Fatalf("record %q: %v", line, err)
	}

	rec, err := zone.NewRecord(zid, zone.MustParseName(fields[0]), class, typ, ttl, fields[4])
	if err != nil {
		t.Fatalf("record %q: %v", line, err)
	}
	rec.ID = zone.RecordID(id.New())
	return &rec
}

// records parses a whole zone's worth of lines.
func records(t testing.TB, zid zone.ZoneID, lines ...string) []*zone.Record {
	t.Helper()
	out := make([]*zone.Record, 0, len(lines))
	for _, line := range lines {
		out = append(out, record(t, zid, line))
	}
	return out
}

// build is the whole path from records to a snapshot, for one zone.
func build(t testing.TB, z *zone.Zone, lines ...string) *Snapshot {
	t.Helper()
	src := source{records: map[zone.ZoneID][]*zone.Record{z.ID: records(t, z.ID, lines...)}}
	snap, err := Build(t.Context(), []*zone.Zone{z}, src)
	if err != nil {
		t.Fatalf("build %q: %v", z.Name, err)
	}
	return snap
}

// tree returns the zone a name falls in, failing the test when there is none.
func tree(t testing.TB, s *Snapshot, name string) *zoneTree {
	t.Helper()
	z := s.zoneFor(zone.MustParseName(name))
	if z == nil {
		t.Fatalf("no zone for %q", name)
	}
	return z
}

// nodeAt returns the node for an owner name, or nil when the zone holds no such
// name.
func nodeAt(tr *zoneTree, name string) *node {
	return tr.lookup(zone.MustParseName(name)).node
}
