package dns

import (
	"errors"
	"strings"
	"testing"

	wire "github.com/miekg/dns"

	"github.com/wegweiserzone/wegweiser/internal/id"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

func TestBuildZoneNodes(t *testing.T) {
	t.Parallel()

	z := newZone(t, "example.com.")
	snap := build(t, z,
		"example.com. 3600 IN NS ns1.example.com.",
		"ns1.example.com. 3600 IN A 192.0.2.1",
		"www.example.com. 300 IN A 192.0.2.10",
		"www.example.com. 300 IN A 192.0.2.11",
		"www.example.com. 300 IN AAAA 2001:db8::10",
		"deep.under.example.com. 300 IN TXT \"here\"",
	)
	tr := tree(t, snap, "example.com.")

	tests := []struct {
		name  string
		owner string
		// sets is the number of RRsets expected at the name; -1 means the name
		// must not exist at all.
		sets int
		why  string
	}{
		{"apex", "example.com.", 2, "the synthesised SOA and the zone's own NS"},
		{"leaf with two types", "www.example.com.", 2, "A and AAAA are separate sets"},
		{"glue name", "ns1.example.com.", 1, "one A"},
		{"empty non-terminal", "under.example.com.", 0, "exists only because something is below it"},
		{"deep leaf", "deep.under.example.com.", 1, ""},
		{"absent", "nothing.example.com.", -1, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n := nodeAt(tr, tc.owner)
			if tc.sets < 0 {
				if n != nil {
					t.Fatalf("%q exists but should not", tc.owner)
				}
				return
			}
			if n == nil {
				t.Fatalf("%q does not exist, but should (%s)", tc.owner, tc.why)
			}
			if got := len(n.sets); got != tc.sets {
				t.Errorf("%q has %d RRsets, want %d (%s)", tc.owner, got, tc.sets, tc.why)
			}
			if got := n.empty(); got != (tc.sets == 0) {
				t.Errorf("%q empty() = %v, want %v", tc.owner, got, tc.sets == 0)
			}
		})
	}

	t.Run("RRset holds every member", func(t *testing.T) {
		t.Parallel()
		n := nodeAt(tr, "www.example.com.")
		set := n.find(zone.ClassIN, zone.TypeA)
		if set == nil {
			t.Fatal("no A RRset at www")
		}
		if got := len(set.rrs); got != 2 {
			t.Errorf("A RRset has %d records, want 2", got)
		}
	})

	t.Run("record count", func(t *testing.T) {
		t.Parallel()
		// Six records plus the SOA the zone contributes.
		if got := snap.Records(); got != 7 {
			t.Errorf("Records() = %d, want 7", got)
		}
	})
}

func TestBuildZoneSOA(t *testing.T) {
	t.Parallel()

	z := newZone(t, "example.com.")
	z.SOA.Serial = zone.NewSerial(4242)

	recs := records(t, z.ID, "example.com. 3600 IN NS ns1.example.com.")
	// A stored SOA is ignored. The model refuses to make one (the serial
	// belongs to the journal, not to whoever last edited the zone) so this can
	// only come from a corrupted row, and two answers that could disagree about
	// the serial are worse than one.
	rdata, err := zone.RDataFromCanonical(
		"ns9.example.com. hostmaster.example.com. 1 3600 900 1209600 3600")
	if err != nil {
		t.Fatal(err)
	}
	recs = append(recs, &zone.Record{
		ID:     zone.RecordID(id.New()),
		ZoneID: z.ID,
		Name:   z.Name,
		Class:  zone.ClassIN,
		Type:   zone.TypeSOA,
		TTL:    3600,
		RData:  rdata,
	})

	snap, err := Build(t.Context(), []*zone.Zone{z},
		source{records: map[zone.ZoneID][]*zone.Record{z.ID: recs}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	set := nodeAt(tree(t, snap, "example.com."), "example.com.").
		find(zone.ClassIN, zone.TypeSOA)
	if set == nil {
		t.Fatal("no SOA at the apex")
	}
	if got := len(set.rrs); got != 1 {
		t.Fatalf("apex carries %d SOA records, want 1", got)
	}
	if got := set.rrs[0].String(); !strings.Contains(got, "4242") {
		t.Errorf("SOA %q does not carry the zone's serial 4242", got)
	}
	if got := set.rrs[0].String(); strings.Contains(got, "ns9") {
		t.Errorf("SOA %q came from the stored record rather than from the zone", got)
	}
}

func TestBuildZoneDelegation(t *testing.T) {
	t.Parallel()

	z := newZone(t, "example.com.")
	snap := build(t, z,
		"example.com. 3600 IN NS ns1.example.com.",
		"ns1.example.com. 3600 IN A 192.0.2.1",
		"sub.example.com. 3600 IN NS ns.sub.example.com.",
		"ns.sub.example.com. 3600 IN A 192.0.2.53",
		"www.example.com. 300 IN A 192.0.2.10",
	)
	tr := tree(t, snap, "example.com.")

	tests := []struct {
		name  string
		owner string
		// delegation is the owner name of the delegation the node is under, or
		// empty when the zone answers for the name itself.
		delegation string
	}{
		{"apex is not a delegation", "example.com.", ""},
		{"apex NS is the zone's own", "ns1.example.com.", ""},
		{"ordinary name", "www.example.com.", ""},
		{"the delegation point itself", "sub.example.com.", "sub.example.com."},
		{"glue below it", "ns.sub.example.com.", "sub.example.com."},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n := nodeAt(tr, tc.owner)
			if n == nil {
				t.Fatalf("%q does not exist", tc.owner)
			}
			switch {
			case tc.delegation == "":
				if n.delegation != nil {
					t.Errorf("%q is under the delegation %q, want none",
						tc.owner, n.delegation.name)
				}
			case n.delegation == nil:
				t.Errorf("%q is under no delegation, want %q", tc.owner, tc.delegation)
			case !n.delegation.name.Equal(zone.MustParseName(tc.delegation)):
				t.Errorf("%q is under the delegation %q, want %q",
					tc.owner, n.delegation.name, tc.delegation)
			}
		})
	}
}

func TestBuildZoneWildcard(t *testing.T) {
	t.Parallel()

	z := newZone(t, "example.com.")
	snap := build(t, z,
		"example.com. 3600 IN NS ns1.example.com.",
		"*.example.com. 300 IN A 192.0.2.99",
		"*.deep.example.com. 300 IN A 192.0.2.98",
		"www.example.com. 300 IN A 192.0.2.10",
	)
	tr := tree(t, snap, "example.com.")

	tests := []struct {
		name   string
		owner  string
		wild   bool
		reason string
	}{
		{"apex carries the wildcard below it", "example.com.", true, ""},
		{"an empty non-terminal can too", "deep.example.com.", true, ""},
		{"an ordinary name does not", "www.example.com.", false, ""},
		{"the wildcard itself has none", "*.example.com.", false, "nothing named * below *"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n := nodeAt(tr, tc.owner)
			if n == nil {
				t.Fatalf("%q does not exist", tc.owner)
			}
			if got := n.wild != nil; got != tc.wild {
				t.Errorf("%q has a wildcard child = %v, want %v %s",
					tc.owner, got, tc.wild, tc.reason)
			}
		})
	}

	t.Run("the wildcard is an ordinary node too", func(t *testing.T) {
		t.Parallel()
		// RFC 4592 §2.1.1: a query for the literal name "*.example.com." is
		// answered by an exact match, not by synthesis.
		n := nodeAt(tr, "*.example.com.")
		if n == nil || n.find(zone.ClassIN, zone.TypeA) == nil {
			t.Fatal("the wildcard name carries no A RRset of its own")
		}
	})
}

func TestBuildSkipsWhatIsOutOfService(t *testing.T) {
	t.Parallel()

	t.Run("disabled record", func(t *testing.T) {
		t.Parallel()
		z := newZone(t, "example.com.")
		recs := records(t, z.ID,
			"example.com. 3600 IN NS ns1.example.com.",
			"gone.example.com. 300 IN A 192.0.2.1",
		)
		recs[1].Disabled = true

		snap, err := Build(t.Context(), []*zone.Zone{z},
			source{records: map[zone.ZoneID][]*zone.Record{z.ID: recs}})
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		if n := nodeAt(tree(t, snap, "example.com."), "gone.example.com."); n != nil {
			t.Error("a disabled record still answers")
		}
	})

	t.Run("disabled zone", func(t *testing.T) {
		t.Parallel()
		z := newZone(t, "example.com.")
		z.Disabled = true

		snap, err := Build(t.Context(), []*zone.Zone{z}, source{})
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		if got := snap.Zones(); got != 0 {
			t.Errorf("Zones() = %d, want 0: a disabled zone must be invisible on the wire", got)
		}
	})
}

func TestBuildErrors(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("the read died")

	tests := []struct {
		name    string
		records func(t *testing.T, zid zone.ZoneID) []*zone.Record
		err     error
		want    string
	}{
		{
			name: "record outside the zone",
			records: func(t *testing.T, zid zone.ZoneID) []*zone.Record {
				return records(t, zid, "www.elsewhere.test. 300 IN A 192.0.2.1")
			},
			want: "outside the zone",
		},
		{
			name: "data the wire library cannot pack",
			records: func(t *testing.T, zid zone.ZoneID) []*zone.Record {
				// Not reachable through zone.NewRecord, which validates. It is
				// reachable through a corrupted row, and the build has to say
				// which record rather than panic on it.
				rdata, err := zone.RDataFromCanonical("not-an-address")
				if err != nil {
					t.Fatal(err)
				}
				return []*zone.Record{{
					ID:     zone.RecordID(id.New()),
					ZoneID: zid,
					Name:   zone.MustParseName("www.example.com."),
					Class:  zone.ClassIN,
					Type:   zone.TypeA,
					TTL:    300,
					RData:  rdata,
				}}
			},
			want: "not-an-address",
		},
		{
			name:    "the store fails mid-stream",
			records: func(t *testing.T, zid zone.ZoneID) []*zone.Record { return nil },
			err:     sentinel,
			want:    "read the records",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			z := newZone(t, "example.com.")
			src := source{
				records: map[zone.ZoneID][]*zone.Record{z.ID: tc.records(t, z.ID)},
				err:     tc.err,
			}

			_, err := Build(t.Context(), []*zone.Zone{z}, src)
			if err == nil {
				t.Fatal("build succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
			if tc.err != nil && !errors.Is(err, tc.err) {
				t.Errorf("error %q does not wrap the store's own", err)
			}
		})
	}
}

// TestTargetName covers the guard rather than the case that occurs: the builder
// only ever asks for the target of a CNAME, so the refusal is what needs
// stating. A type that reaches it without a case is a caller the builder was
// not written for, and failing the rebuild is better than answering with a zero
// name that resolves to nothing.
func TestTargetName(t *testing.T) {
	t.Parallel()

	cname, err := wire.NewRR("alias.example.com. 300 IN CNAME www.example.com.")
	if err != nil {
		t.Fatalf("new RR: %v", err)
	}
	target, err := targetName(cname)
	if err != nil {
		t.Fatalf("target of a CNAME: %v", err)
	}
	if want := zone.MustParseName("www.example.com."); !target.Equal(want) {
		t.Errorf("target = %q, want %q", target, want)
	}

	a, err := wire.NewRR("www.example.com. 300 IN A 192.0.2.1")
	if err != nil {
		t.Fatalf("new RR: %v", err)
	}
	if _, err := targetName(a); err == nil {
		t.Error("an A record reported a target name, and it has none")
	}
}

// TestRebuild covers the path a server actually starts on: everything the store
// holds, read once, with no list of zones supplied from outside.
func TestRebuild(t *testing.T) {
	t.Parallel()

	com := newZone(t, "example.com.")
	net := newZone(t, "example.net.")
	off := newZone(t, "off.example.")
	off.Disabled = true

	src := source{
		zones: []*zone.Zone{com, net, off},
		records: map[zone.ZoneID][]*zone.Record{
			com.ID: records(t, com.ID, "www.example.com. 300 IN A 192.0.2.1"),
			net.ID: records(t, net.ID, "www.example.net. 300 IN A 198.51.100.1"),
			off.ID: records(t, off.ID, "www.off.example. 300 IN A 192.0.2.9"),
		},
	}

	snap, err := Rebuild(t.Context(), src)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	// Two zones, not three: a disabled zone is out of service, which has to
	// mean invisible on the wire rather than merely marked.
	if snap.Zones() != 2 {
		t.Errorf("the snapshot holds %d zones, want 2", snap.Zones())
	}
	if snap.zoneAt(off.Name) != nil {
		t.Error("a disabled zone reached the snapshot")
	}
	// One record each plus the SOA the builder synthesises.
	if snap.Records() != 4 {
		t.Errorf("the snapshot holds %d records, want 4", snap.Records())
	}

	var a Answer
	snap.Resolve(Question{
		Name: zone.MustParseName("www.example.net."), Class: zone.ClassIN, Type: zone.TypeA,
	}, &a)
	if a.Rcode != 0 || len(a.Answer) != 1 {
		t.Errorf("rcode = %d with %d records, want NOERROR with 1", a.Rcode, len(a.Answer))
	}
}

// TestRebuildEmpty checks that a store with nothing in it produces a snapshot
// that answers for nothing, rather than a nil one a caller has to guard.
func TestRebuildEmpty(t *testing.T) {
	t.Parallel()

	snap, err := Rebuild(t.Context(), source{})
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if snap == nil || snap.Zones() != 0 {
		t.Fatalf("an empty store produced %v", snap)
	}
}

// TestRebuildStreamFails checks that a zone stream dying partway is reported
// rather than producing a snapshot of whatever arrived before it. A half-built
// snapshot would answer NXDOMAIN for zones this server holds.
func TestRebuildStreamFails(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("the database went away")
	z := newZone(t, "example.com.")
	src := source{zones: []*zone.Zone{z}, zoneErr: sentinel}

	_, err := Rebuild(t.Context(), src)
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want it to wrap the store's own", err)
	}
	if !strings.Contains(err.Error(), "read the zones") {
		t.Errorf("error %q does not say which read failed", err)
	}
}
