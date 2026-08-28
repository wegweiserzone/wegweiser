package dns

import (
	"strings"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

func TestZoneTreeLookup(t *testing.T) {
	t.Parallel()

	z := newZone(t, "example.com.")
	tr := tree(t, build(t, z,
		"example.com. 3600 IN NS ns1.example.com.",
		"ns1.example.com. 3600 IN A 192.0.2.1",
		"www.example.com. 300 IN A 192.0.2.10",
		"deep.under.example.com. 300 IN TXT \"here\"",
		"sub.example.com. 3600 IN NS ns.sub.example.com.",
		"ns.sub.example.com. 3600 IN A 192.0.2.53",
	), "example.com.")

	tests := []struct {
		name  string
		qname string
		// node, closest and delegation are the owner names expected, empty for
		// nil.
		node       string
		closest    string
		delegation string
		why        string
	}{
		{
			name: "the apex itself", qname: "example.com.",
			node: "example.com.", closest: "example.com.",
		},
		{
			name: "a name that exists", qname: "www.example.com.",
			node: "www.example.com.", closest: "www.example.com.",
		},
		{
			name: "an empty non-terminal", qname: "under.example.com.",
			node: "under.example.com.", closest: "under.example.com.",
			why: "it exists, so this is NODATA rather than NXDOMAIN",
		},
		{
			name: "a name that does not exist", qname: "nothing.example.com.",
			node: "", closest: "example.com.",
			why: "the closest encloser is where wildcard synthesis starts",
		},
		{
			name: "below a name that does exist", qname: "x.deep.under.example.com.",
			node: "", closest: "deep.under.example.com.",
		},
		{
			name: "the delegation point", qname: "sub.example.com.",
			node: "sub.example.com.", closest: "sub.example.com.",
			delegation: "sub.example.com.",
			why:        "a query for the delegated name is referred, not answered",
		},
		{
			name: "glue below a delegation", qname: "ns.sub.example.com.",
			node: "ns.sub.example.com.", closest: "ns.sub.example.com.",
			delegation: "sub.example.com.",
			why:        "glue is not authoritative data of this zone",
		},
		{
			name: "a name that does not exist below a delegation", qname: "any.sub.example.com.",
			node: "", closest: "sub.example.com.", delegation: "sub.example.com.",
			why: "we cannot say a name is absent from a zone we handed away",
		},
		{
			name: "a name outside the zone", qname: "elsewhere.test.",
			node: "", closest: "",
			why: "zone selection happens before this",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tr.lookup(zone.MustParseName(tc.qname))

			for _, f := range []struct {
				field string
				got   *node
				want  string
			}{
				{"node", got.node, tc.node},
				{"closest", got.closest, tc.closest},
				{"delegation", got.delegation, tc.delegation},
			} {
				switch {
				case f.want == "":
					if f.got != nil {
						t.Errorf("%s = %q, want none (%s)", f.field, f.got.name, tc.why)
					}
				case f.got == nil:
					t.Errorf("%s = none, want %q (%s)", f.field, f.want, tc.why)
				case !f.got.name.Equal(zone.MustParseName(f.want)):
					t.Errorf("%s = %q, want %q (%s)", f.field, f.got.name, f.want, tc.why)
				}
			}
		})
	}
}

func TestZoneTreeLookupBoundsTheName(t *testing.T) {
	t.Parallel()

	// A query name is cut back to the depth of the deepest name the zone holds
	// before the walk begins. That is a bound on the work one packet can buy
	// (§2.8), and it must not change any answer: everything below the deepest
	// name in the zone is absent either way, and the closest encloser is at or
	// above it by definition.
	z := newZone(t, "example.com.")
	tr := tree(t, build(t, z,
		"example.com. 3600 IN NS ns1.example.com.",
		"a.b.example.com. 300 IN A 192.0.2.1",
	), "example.com.")

	if tr.depth != 2 {
		t.Fatalf("zone depth = %d, want 2", tr.depth)
	}

	tests := []struct {
		name    string
		qname   string
		closest string
	}{
		{
			name:  "far below a name that exists",
			qname: "x.y.z.w.a.b.example.com.", closest: "a.b.example.com.",
		},
		{
			name:  "far below a name that does not",
			qname: "x.y.z.nothing.example.com.", closest: "example.com.",
		},
		{
			name:    "a name at the maximum label count",
			qname:   strings.Repeat("x.", 100) + "a.b.example.com.",
			closest: "a.b.example.com.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := tr.lookup(zone.MustParseName(tc.qname))
			if got.node != nil {
				t.Errorf("node = %q, want none: the name is below everything the zone holds",
					got.node.name)
			}
			if got.closest == nil {
				t.Fatalf("closest = none, want %q", tc.closest)
			}
			if !got.closest.name.Equal(zone.MustParseName(tc.closest)) {
				t.Errorf("closest = %q, want %q", got.closest.name, tc.closest)
			}
		})
	}
}

// TestZoneTreeLookupIsAllocationFree does not call t.Parallel: AllocsPerRun
// measures the whole process and is meaningless while other tests allocate.
func TestZoneTreeLookupIsAllocationFree(t *testing.T) {
	// The walk runs on every query, and D12 budgets no allocations beyond the
	// response buffer. Stepping up a name is a substring of the same backing
	// array, so this holds for a miss as well as for a hit.
	z := newZone(t, "example.com.")
	tr := tree(t, build(t, z,
		"example.com. 3600 IN NS ns1.example.com.",
		"a.b.c.d.example.com. 300 IN A 192.0.2.1",
	), "example.com.")

	for _, qname := range []string{
		"example.com.",
		"a.b.c.d.example.com.",
		"nothing.b.c.d.example.com.",
	} {
		n := zone.MustParseName(qname)
		if got := testing.AllocsPerRun(100, func() { tr.lookup(n) }); got != 0 {
			t.Errorf("lookup(%q) allocates %v times per call, want 0", qname, got)
		}
	}
}

func TestNodeFind(t *testing.T) {
	t.Parallel()

	z := newZone(t, "example.com.")
	tr := tree(t, build(t, z,
		"example.com. 3600 IN NS ns1.example.com.",
		"www.example.com. 300 IN A 192.0.2.10",
	), "example.com.")
	n := nodeAt(tr, "www.example.com.")

	if set := n.find(zone.ClassIN, zone.TypeA); set == nil {
		t.Error("the A RRset is not found by its own class and type")
	}
	if set := n.find(zone.ClassIN, zone.TypeAAAA); set != nil {
		t.Error("a type that is not there was found")
	}
	// Class is part of the key. A CHAOS query must not be answered from the
	// internet class, however unlikely one is.
	if set := n.find(zone.ClassCH, zone.TypeA); set != nil {
		t.Error("an RRset was found under the wrong class")
	}
}
