package dns

import (
	"strings"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

func TestSnapshotZoneFor(t *testing.T) {
	t.Parallel()

	root := newZone(t, ".")
	parent := newZone(t, "example.com.")
	child := newZone(t, "sub.example.com.")

	src := source{records: map[zone.ZoneID][]*zone.Record{
		root.ID:   records(t, root.ID, ". 3600 IN NS ns1.example.com."),
		parent.ID: records(t, parent.ID, "example.com. 3600 IN NS ns1.example.com."),
		child.ID:  records(t, child.ID, "sub.example.com. 3600 IN NS ns1.example.com."),
	}}
	snap, err := Build(t.Context(), []*zone.Zone{root, parent, child}, src)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	tests := []struct {
		name  string
		qname string
		want  string
		why   string
	}{
		{"the apex of a zone", "example.com.", "example.com.", ""},
		{"a name inside it", "www.example.com.", "example.com.", ""},
		{"a name in the child zone", "www.sub.example.com.", "sub.example.com.",
			"the longest match wins, so the child answers for its own namespace"},
		{"the child apex", "sub.example.com.", "sub.example.com.", ""},
		{"a name we hold no zone for", "www.elsewhere.test.", ".",
			"the root zone covers everything once it exists"},
		{"a name that only looks like one of ours", "notexample.com.", ".",
			"suffix matching respects label boundaries"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := snap.zoneFor(zone.MustParseName(tc.qname))
			if got == nil {
				t.Fatalf("no zone for %q, want %q (%s)", tc.qname, tc.want, tc.why)
			}
			if !got.name.Equal(zone.MustParseName(tc.want)) {
				t.Errorf("zone for %q = %q, want %q (%s)", tc.qname, got.name, tc.want, tc.why)
			}
		})
	}
}

func TestSnapshotZoneForBoundsTheName(t *testing.T) {
	t.Parallel()

	// The same bound as inside a zone: labels below the deepest apex cannot
	// select a zone, so they are dropped before the walk. The answer must not
	// depend on how long the query was.
	z := newZone(t, "example.com.")
	snap := build(t, z, "example.com. 3600 IN NS ns1.example.com.")

	for _, qname := range []string{
		"a.b.c.d.e.f.g.example.com.",
		strings.Repeat("x.", 100) + "example.com.",
	} {
		got := snap.zoneFor(zone.MustParseName(qname))
		if got == nil {
			t.Errorf("no zone for %q, want %q", qname, z.Name)
			continue
		}
		if !got.name.Equal(z.Name) {
			t.Errorf("zone for %q = %q, want %q", qname, got.name, z.Name)
		}
	}
}

func TestSnapshotZoneForRefuses(t *testing.T) {
	t.Parallel()

	// Without a root zone, a name we hold nothing for has no zone at all. That
	// is REFUSED on the wire and not NXDOMAIN: a server cannot assert that a
	// name is absent from a namespace it does not serve.
	z := newZone(t, "example.com.")
	snap := build(t, z, "example.com. 3600 IN NS ns1.example.com.")

	for _, qname := range []string{"elsewhere.test.", "com.", "."} {
		if got := snap.zoneFor(zone.MustParseName(qname)); got != nil {
			t.Errorf("zone for %q = %q, want none", qname, got.name)
		}
	}
}

func TestSnapshotWithZone(t *testing.T) {
	t.Parallel()

	first := newZone(t, "example.com.")
	second := newZone(t, "example.net.")
	src := source{records: map[zone.ZoneID][]*zone.Record{
		first.ID: records(t, first.ID,
			"example.com. 3600 IN NS ns1.example.com.",
			"www.example.com. 300 IN A 192.0.2.10",
		),
		second.ID: records(t, second.ID, "example.net. 3600 IN NS ns1.example.net."),
	}}

	base, err := Build(t.Context(), []*zone.Zone{first, second}, src)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	t.Run("the other zones are carried over by pointer", func(t *testing.T) {
		t.Parallel()
		next, err := base.WithZone(t.Context(), first, src)
		if err != nil {
			t.Fatalf("with zone: %v", err)
		}
		if next == base {
			t.Fatal("WithZone returned the receiver; a snapshot is immutable")
		}
		if next.zoneAt(second.Name) != base.zoneAt(second.Name) {
			t.Error("the untouched zone was rebuilt instead of shared")
		}
		if next.zoneAt(first.Name) == base.zoneAt(first.Name) {
			t.Error("the changed zone was shared instead of rebuilt")
		}
	})

	t.Run("the receiver goes on serving the old state", func(t *testing.T) {
		t.Parallel()
		shrunk := source{records: map[zone.ZoneID][]*zone.Record{
			first.ID: records(t, first.ID, "example.com. 3600 IN NS ns1.example.com."),
		}}
		next, err := base.WithZone(t.Context(), first, shrunk)
		if err != nil {
			t.Fatalf("with zone: %v", err)
		}

		const www = "www.example.com."
		if nodeAt(base.zoneAt(first.Name), www) == nil {
			t.Error("the old snapshot lost a record it was still serving")
		}
		if nodeAt(next.zoneAt(first.Name), www) != nil {
			t.Error("the new snapshot still serves a record that is gone")
		}
		if got, want := next.Records(), base.Records()-1; got != want {
			t.Errorf("Records() = %d, want %d", got, want)
		}
	})

	t.Run("a zone added for the first time", func(t *testing.T) {
		t.Parallel()
		third := newZone(t, "example.org.")
		next, err := base.WithZone(t.Context(), third, source{})
		if err != nil {
			t.Fatalf("with zone: %v", err)
		}
		if next.Zones() != base.Zones()+1 {
			t.Errorf("Zones() = %d, want %d", next.Zones(), base.Zones()+1)
		}
		if base.zoneFor(third.Name) != nil {
			t.Error("the new zone appeared in the old snapshot")
		}
	})

	t.Run("a zone that has been disabled is removed", func(t *testing.T) {
		t.Parallel()
		off := *first
		off.Disabled = true
		next, err := base.WithZone(t.Context(), &off, src)
		if err != nil {
			t.Fatalf("with zone: %v", err)
		}
		if next.zoneFor(first.Name) != nil {
			t.Error("a disabled zone still answers")
		}
		if next.Zones() != base.Zones()-1 {
			t.Errorf("Zones() = %d, want %d", next.Zones(), base.Zones()-1)
		}
	})

	t.Run("a failed build changes nothing", func(t *testing.T) {
		t.Parallel()
		broken := newZone(t, "example.com.")
		broken.ID = first.ID
		bad := source{records: map[zone.ZoneID][]*zone.Record{
			first.ID: records(t, first.ID, "www.elsewhere.test. 300 IN A 192.0.2.1"),
		}}
		if _, err := base.WithZone(t.Context(), broken, bad); err == nil {
			t.Fatal("the build succeeded, want an error")
		}
		if base.Zones() != 2 {
			t.Error("the receiver was modified by a build that failed")
		}
	})
}

func TestSnapshotWithoutZone(t *testing.T) {
	t.Parallel()

	z := newZone(t, "example.com.")
	base := build(t, z, "example.com. 3600 IN NS ns1.example.com.")

	next := base.WithoutZone(z.Name)
	if next.Zones() != 0 {
		t.Errorf("Zones() = %d, want 0", next.Zones())
	}
	if next.Records() != 0 {
		t.Errorf("Records() = %d, want 0", next.Records())
	}
	if base.Zones() != 1 {
		t.Error("the receiver lost the zone as well")
	}

	if got := base.WithoutZone(zone.MustParseName("elsewhere.test.")); got != base {
		t.Error("removing a zone that is not there built a new snapshot for nothing")
	}
}

func TestNilSnapshotAnswersForNothing(t *testing.T) {
	t.Parallel()

	// A server is wired up before its first build. Reading through a nil
	// snapshot has to be as ordinary as reading through an empty one, or every
	// query path grows a nil check.
	var s *Snapshot

	if s.Zones() != 0 || s.Records() != 0 {
		t.Error("a nil snapshot reports content")
	}
	if s.zoneFor(zone.MustParseName("example.com.")) != nil {
		t.Error("a nil snapshot found a zone")
	}
	if s.WithoutZone(zone.MustParseName("example.com.")) != nil {
		t.Error("removing from a nil snapshot built one")
	}

	z := newZone(t, "example.com.")
	next, err := s.WithZone(t.Context(), z, source{})
	if err != nil {
		t.Fatalf("with zone: %v", err)
	}
	if next.Zones() != 1 {
		t.Errorf("Zones() = %d, want 1", next.Zones())
	}
}
