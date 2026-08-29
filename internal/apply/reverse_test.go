package apply_test

import (
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/apply"
	"github.com/wegweiserzone/wegweiser/internal/id"
	"github.com/wegweiserzone/wegweiser/internal/journal"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// reverseZone creates a reverse zone alongside the fixture's forward zone.
func (f *fixture) reverseZone(apex string) *zone.Zone {
	f.t.Helper()

	z := newZone(f.t, apex)
	ns, err := zone.NewRecord(z.ID, z.Name, zone.ClassIN, zone.TypeNS, 3600, "ns1.example.com.")
	if err != nil {
		f.t.Fatalf("NewRecord: %v", err)
	}
	if _, err := f.a.CreateZone(f.t.Context(), z, []zone.Record{ns}, testMeta()); err != nil {
		f.t.Fatalf("CreateZone(%s): %v", apex, err)
	}
	return z
}

// ptrs renders the PTR records of a zone as "owner -> target" lines.
func (f *fixture) ptrs(z *zone.Zone) []string {
	f.t.Helper()

	var out []string
	for r, err := range f.s.IterZoneRecords(f.t.Context(), z.ID) {
		if err != nil {
			f.t.Fatalf("IterZoneRecords: %v", err)
		}
		if r.Type == zone.TypePTR {
			out = append(out, r.Name.String()+" -> "+r.RData.String())
		}
	}
	return out
}

func (f *fixture) addA(name, addr string) *journal.Commit {
	f.t.Helper()
	return f.mustApply(f.command(apply.RecordOp{
		Action: apply.ActionAdd, Record: f.record(name, zone.TypeA, 3600, addr),
	}))
}

// storeA writes an address record straight into the database, past the applier
// and so past reverse automation. It is how a test reaches the state the write
// path no longer produces: an address with no entry generated for it.
func (f *fixture) storeA(name, addr string) zone.Record {
	f.t.Helper()

	rec, err := zone.NewRecord(f.z.ID, zone.MustParseName(name),
		zone.ClassIN, zone.TypeA, 3600, addr)
	if err != nil {
		f.t.Fatalf("NewRecord(%s %s): %v", name, addr, err)
	}
	rec.ID = zone.RecordID(id.New())
	if uerr := f.s.Update(f.t.Context(), func(tx store.Tx) error {
		return tx.InsertRecord(f.t.Context(), &rec)
	}); uerr != nil {
		f.t.Fatalf("insert behind the applier's back: %v", uerr)
	}
	return rec
}

func TestReverseGeneration(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rev := f.reverseZone("2.0.192.in-addr.arpa.")

	res, err := f.a.Apply(t.Context(), f.command(apply.RecordOp{
		Action: apply.ActionAdd, Record: f.record("www.example.com.", zone.TypeA, 3600, "192.0.2.10"),
	}))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// One change, two zones, two commits: zones carry independent serials, so
	// one commit could not advance both.
	if len(res.Commits) != 2 {
		t.Fatalf("the change produced %d commits, want two", len(res.Commits))
	}
	if res.Commits[0].ZoneID != f.z.ID {
		t.Errorf("the first commit is for %q, want the zone that was addressed", res.Commits[0].ZoneName)
	}
	second := res.Commits[1]
	if second.ZoneID != rev.ID {
		t.Errorf("the second commit is for %q, want %q", second.ZoneName, rev.Name)
	}
	// Nobody edited the reverse zone; the server did, on their behalf.
	if second.Source != journal.SourceSystem {
		t.Errorf("the reverse commit says it came from %q, want %q", second.Source, journal.SourceSystem)
	}
	if got := eventLines(second); !slices.Equal(got,
		[]string{"+10.2.0.192.in-addr.arpa.\t3600\tIN\tPTR\twww.example.com."}) {
		t.Errorf("reverse events %v", got)
	}

	if got := f.ptrs(rev); !slices.Equal(got, []string{"10.2.0.192.in-addr.arpa. -> www.example.com."}) {
		t.Errorf("the reverse zone holds %v", got)
	}

	t.Run("the entry knows where it came from", func(t *testing.T) {
		page, err := f.s.ListRecords(t.Context(), store.RecordFilter{
			ZoneID: rev.ID, Types: []zone.RRType{zone.TypePTR},
		})
		if err != nil {
			t.Fatalf("ListRecords: %v", err)
		}
		if len(page.Items) != 1 {
			t.Fatalf("the reverse zone holds %d PTR records, want one", len(page.Items))
		}
		ptr := page.Items[0]
		if !ptr.IsManaged() || ptr.ManagedKind != zone.ManagedPTR {
			t.Errorf("the entry is not marked as generated: %+v", ptr)
		}
		derived, err := f.s.ManagedBy(t.Context(), ptr.ManagedBy)
		if err != nil || len(derived) != 1 || derived[0].ID != ptr.ID {
			t.Errorf("the source does not point back at the entry: %v", err)
		}
	})

	t.Run("applying the same change again does nothing", func(t *testing.T) {
		before := f.serial()
		res, err := f.a.Apply(t.Context(), f.command(apply.RecordOp{
			Action:  apply.ActionReplaceRRset,
			Key:     zone.RRsetKey{Name: zone.MustParseName("www.example.com."), Class: zone.ClassIN, Type: zone.TypeA},
			Records: []zone.Record{*f.record("www.example.com.", zone.TypeA, 3600, "192.0.2.10")},
		}))
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if res.Changed() {
			t.Errorf("re-applying the same state produced %d commits", len(res.Commits))
		}
		if got := f.serial(); got != before {
			t.Errorf("the serial moved from %s to %s", before, got)
		}
	})
}

// The generated entry has to follow the record it came from, or the reverse
// zone slowly fills with answers for addresses nothing uses any more.
func TestReverseFollowsItsSource(t *testing.T) {
	t.Parallel()

	t.Run("an address change moves the entry", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		rev := f.reverseZone("2.0.192.in-addr.arpa.")

		f.addA("www.example.com.", "192.0.2.10")
		rec := f.recordAt("www.example.com.", zone.TypeA, "192.0.2.10")

		res, err := f.a.Apply(t.Context(), f.command(apply.RecordOp{
			Action:   apply.ActionUpdate,
			RecordID: rec.ID,
			Record:   f.record("www.example.com.", zone.TypeA, 3600, "192.0.2.20"),
		}))
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if got := f.ptrs(rev); !slices.Equal(got, []string{"20.2.0.192.in-addr.arpa. -> www.example.com."}) {
			t.Errorf("the reverse zone holds %v", got)
		}
		// The removal and the arrival are one commit in the reverse zone, in
		// the order a difference sequence needs.
		if len(res.Commits) != 2 {
			t.Fatalf("the change produced %d commits, want two", len(res.Commits))
		}
		if got := eventLines(res.Commits[1]); len(got) != 2 || got[0][0] != '-' || got[1][0] != '+' {
			t.Errorf("reverse events %v, want a removal then an arrival", got)
		}
	})

	t.Run("a name change rewrites the entry", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		rev := f.reverseZone("2.0.192.in-addr.arpa.")

		f.addA("www.example.com.", "192.0.2.10")
		rec := f.recordAt("www.example.com.", zone.TypeA, "192.0.2.10")

		if _, err := f.a.Apply(t.Context(), f.command(apply.RecordOp{
			Action:   apply.ActionUpdate,
			RecordID: rec.ID,
			Record:   f.record("web.example.com.", zone.TypeA, 3600, "192.0.2.10"),
		})); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if got := f.ptrs(rev); !slices.Equal(got, []string{"10.2.0.192.in-addr.arpa. -> web.example.com."}) {
			t.Errorf("the reverse zone holds %v", got)
		}
	})

	t.Run("deleting the source removes the entry, with an event", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		rev := f.reverseZone("2.0.192.in-addr.arpa.")

		f.addA("www.example.com.", "192.0.2.10")
		rec := f.recordAt("www.example.com.", zone.TypeA, "192.0.2.10")

		res, err := f.a.Apply(t.Context(), f.command(apply.RecordOp{
			Action: apply.ActionDelete, RecordID: rec.ID,
		}))
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if got := f.ptrs(rev); len(got) != 0 {
			t.Errorf("the reverse zone still holds %v", got)
		}
		// The database would cascade this away on its own. That is exactly the
		// problem: a row vanishing without an event is a change the reverse
		// zone's journal never saw, and its secondaries would never learn of.
		if len(res.Commits) != 2 {
			t.Fatalf("the deletion produced %d commits, want two", len(res.Commits))
		}
		if got := eventLines(res.Commits[1]); !slices.Equal(got,
			[]string{"-10.2.0.192.in-addr.arpa.\t3600\tIN\tPTR\twww.example.com."}) {
			t.Errorf("reverse events %v", got)
		}
	})

	t.Run("an edit to something else leaves it alone", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		rev := f.reverseZone("2.0.192.in-addr.arpa.")

		f.addA("www.example.com.", "192.0.2.10")
		rec := f.recordAt("www.example.com.", zone.TypeA, "192.0.2.10")
		ptrBefore := f.ptrs(rev)

		next := f.record("www.example.com.", zone.TypeA, 3600, "192.0.2.10")
		next.Comment = "the web server"
		res, err := f.a.Apply(t.Context(), f.command(apply.RecordOp{
			Action: apply.ActionUpdate, RecordID: rec.ID, Record: next,
		}))
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if len(res.Commits) != 1 {
			t.Errorf("a comment change reached %d zones, want one", len(res.Commits))
		}
		if got := f.ptrs(rev); !slices.Equal(got, ptrBefore) {
			t.Errorf("the reverse zone changed from %v to %v", ptrBefore, got)
		}
	})
}

// RFC 2317 §4: a classless child is not an ancestor of the plain reverse name,
// so the entry goes under the child's own apex, and the longest prefix wins.
func TestReverseClasslessAndIPv6(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	f.reverseZone("2.0.192.in-addr.arpa.")
	classless := f.reverseZone("0/25.2.0.192.in-addr.arpa.")
	v6 := f.reverseZone("8.b.d.0.1.0.0.2.ip6.arpa.")

	f.addA("low.example.com.", "192.0.2.10")   // inside the classless child
	f.addA("high.example.com.", "192.0.2.200") // only in the /24

	if got := f.ptrs(classless); !slices.Equal(got,
		[]string{"10.0/25.2.0.192.in-addr.arpa. -> low.example.com."}) {
		t.Errorf("the classless child holds %v", got)
	}

	f.mustApply(f.command(apply.RecordOp{
		Action: apply.ActionAdd,
		Record: f.record("v6.example.com.", zone.TypeAAAA, 3600, "2001:db8::1"),
	}))
	if got := f.ptrs(v6); len(got) != 1 {
		t.Errorf("the IPv6 reverse zone holds %v, want one entry", got)
	}

	// The /24 got the address the child does not cover, and only that one.
	parent, err := f.s.ZoneByName(t.Context(), zone.MustParseName("2.0.192.in-addr.arpa."))
	if err != nil {
		t.Fatalf("ZoneByName: %v", err)
	}
	if got := f.ptrs(parent); !slices.Equal(got,
		[]string{"200.2.0.192.in-addr.arpa. -> high.example.com."}) {
		t.Errorf("the /24 holds %v", got)
	}
}

// D3: several names pointing at one address is the normal case. The entry that
// is there stays, and the caller is told: a conflict only in the server log is
// the same as no conflict detection at all.
func TestReverseConflictPolicies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		policy apply.Policy
		want   []string
	}{
		{apply.PolicyFirstWins, []string{"10.2.0.192.in-addr.arpa. -> first.example.com."}},
		{apply.PolicyLastWins, []string{"10.2.0.192.in-addr.arpa. -> second.example.com."}},
		{apply.PolicyMulti, []string{
			"10.2.0.192.in-addr.arpa. -> first.example.com.",
			"10.2.0.192.in-addr.arpa. -> second.example.com.",
		}},
	}

	for _, tc := range tests {
		t.Run(string(tc.policy), func(t *testing.T) {
			t.Parallel()
			f := newFixtureWith(t, apply.Options{Policy: tc.policy})
			rev := f.reverseZone("2.0.192.in-addr.arpa.")

			f.addA("first.example.com.", "192.0.2.10")
			res, err := f.a.Apply(t.Context(), f.command(apply.RecordOp{
				Action: apply.ActionAdd,
				Record: f.record("second.example.com.", zone.TypeA, 3600, "192.0.2.10"),
			}))
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}

			got := f.ptrs(rev)
			slices.Sort(got)
			want := slices.Clone(tc.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("the reverse zone holds\n  got  %v\n  want %v", got, want)
			}

			// The forward record is written either way: a reverse conflict is
			// not a reason to refuse the change someone actually asked for.
			if _, err := f.s.ZoneByID(t.Context(), f.z.ID); err != nil {
				t.Fatalf("ZoneByID: %v", err)
			}
			f.recordAt("second.example.com.", zone.TypeA, "192.0.2.10")

			if tc.policy == apply.PolicyMulti {
				if len(res.Conflicts) != 0 {
					t.Errorf("multi reported %d conflicts, want none", len(res.Conflicts))
				}
				return
			}
			if len(res.Conflicts) != 1 {
				t.Fatalf("the change reported %d conflicts, want one", len(res.Conflicts))
			}
			c := res.Conflicts[0]
			if c.Address != netip.MustParseAddr("192.0.2.10") ||
				c.Existing.String() != "first.example.com." ||
				c.SourceName.String() != "second.example.com." ||
				!c.Generated || c.Policy != tc.policy {
				t.Errorf("the conflict reads as %+v", c)
			}
		})
	}

	t.Run("reject refuses the change", func(t *testing.T) {
		t.Parallel()
		f := newFixtureWith(t, apply.Options{Policy: apply.PolicyReject})
		f.reverseZone("2.0.192.in-addr.arpa.")

		f.addA("first.example.com.", "192.0.2.10")
		_, err := f.a.Apply(t.Context(), f.command(apply.RecordOp{
			Action: apply.ActionAdd,
			Record: f.record("second.example.com.", zone.TypeA, 3600, "192.0.2.10"),
		}))
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("Apply = %v, want an error wrapping store.ErrConflict", err)
		}
		// The whole command is one transaction, so the forward record is gone
		// too.
		page, perr := f.s.ListRecords(t.Context(), store.RecordFilter{
			ZoneID: f.z.ID, Name: zone.MustParseName("second.example.com."),
		})
		if perr != nil {
			t.Fatalf("ListRecords: %v", perr)
		}
		if len(page.Items) != 0 {
			t.Errorf("the refused command left the forward record behind")
		}
	})

	// Two records added in one command have to see each other, or the second
	// would quietly overwrite the first inside a single transaction.
	t.Run("two names in one command conflict with each other", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		rev := f.reverseZone("2.0.192.in-addr.arpa.")

		res, err := f.a.Apply(t.Context(), f.command(
			apply.RecordOp{Action: apply.ActionAdd, Record: f.record("a.example.com.", zone.TypeA, 3600, "192.0.2.10")},
			apply.RecordOp{Action: apply.ActionAdd, Record: f.record("b.example.com.", zone.TypeA, 3600, "192.0.2.10")},
		))
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if got := f.ptrs(rev); !slices.Equal(got, []string{"10.2.0.192.in-addr.arpa. -> a.example.com."}) {
			t.Errorf("the reverse zone holds %v", got)
		}
		if len(res.Conflicts) != 1 {
			t.Errorf("the command reported %d conflicts, want one", len(res.Conflicts))
		}
	})
}

// D6: creating a zone is an assertion of authority over a namespace. Doing it
// as a side effect of adding a record would be a surprise, and for public
// address space it would be wrong.
func TestReverseMissingZoneIsOfferedNotCreated(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	res, err := f.a.Apply(t.Context(), f.command(apply.RecordOp{
		Action: apply.ActionAdd, Record: f.record("www.example.com.", zone.TypeA, 3600, "192.0.2.10"),
	}))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// The forward record went in; only the reverse entry had nowhere to go.
	f.recordAt("www.example.com.", zone.TypeA, "192.0.2.10")
	if len(res.Commits) != 1 {
		t.Errorf("the change reached %d zones, want one", len(res.Commits))
	}

	if len(res.MissingZones) != 1 {
		t.Fatalf("the change reported %d missing zones, want one", len(res.MissingZones))
	}
	m := res.MissingZones[0]
	if m.Suggested.String() != "2.0.192.in-addr.arpa." {
		t.Errorf("the suggestion is %q, want the /24", m.Suggested)
	}
	if m.Prefix != netip.MustParsePrefix("192.0.2.0/24") || m.SourceName.String() != "www.example.com." {
		t.Errorf("the hint reads as %+v", m)
	}
	// Nothing was created behind the caller's back.
	if _, err := f.s.ZoneByName(t.Context(), m.Suggested); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the reverse zone was created anyway: %v", err)
	}

	t.Run("IPv6 is suggested at the /64", func(t *testing.T) {
		res, err := f.a.Apply(t.Context(), f.command(apply.RecordOp{
			Action: apply.ActionAdd, Record: f.record("v6.example.com.", zone.TypeAAAA, 3600, "2001:db8::1"),
		}))
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if len(res.MissingZones) != 1 {
			t.Fatalf("the change reported %d missing zones, want one", len(res.MissingZones))
		}
		if got := res.MissingZones[0].Prefix; got != netip.MustParsePrefix("2001:db8::/64") {
			t.Errorf("the suggested network is %v, want a /64", got)
		}
	})
}

// D4: editing a generated entry is refused, and detaching is the way to take it
// over. A detached entry is an ordinary record the automation stops touching.
func TestReverseDetach(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rev := f.reverseZone("2.0.192.in-addr.arpa.")

	f.addA("www.example.com.", "192.0.2.10")
	page, err := f.s.ListRecords(t.Context(), store.RecordFilter{
		ZoneID: rev.ID, Types: []zone.RRType{zone.TypePTR},
	})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("ListRecords: %v", err)
	}
	ptr := page.Items[0]

	revCommand := func(ops ...apply.RecordOp) apply.Command {
		return apply.Command{
			ZoneID: rev.ID, Ops: ops,
			Kind: journal.KindEdit, Source: journal.SourceAPI, Actor: "tim",
		}
	}

	t.Run("editing it is refused, and says what to do instead", func(t *testing.T) {
		_, err := f.a.Apply(t.Context(), revCommand(apply.RecordOp{
			Action: apply.ActionDelete, RecordID: ptr.ID,
		}))
		if !errors.Is(err, zone.ErrInvalid) {
			t.Fatalf("Apply = %v, want an error wrapping zone.ErrInvalid", err)
		}
	})

	t.Run("detaching makes it an ordinary record", func(t *testing.T) {
		res, err := f.a.Apply(t.Context(), revCommand(apply.RecordOp{
			Action: apply.ActionDetach, RecordID: ptr.ID,
		}))
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if !res.Changed() {
			t.Fatal("detaching produced no commit")
		}

		got, err := f.s.RecordByID(t.Context(), ptr.ID)
		if err != nil {
			t.Fatalf("RecordByID: %v", err)
		}
		if got.IsManaged() {
			t.Errorf("the record is still marked as generated: %+v", got)
		}
		if got.RData.String() != ptr.RData.String() {
			t.Errorf("detaching changed the data to %q", got.RData)
		}

		// Detaching again is not an error: it has got what it asked for.
		if _, err := f.a.Apply(t.Context(), revCommand(apply.RecordOp{
			Action: apply.ActionDetach, RecordID: ptr.ID,
		})); err != nil {
			t.Errorf("detaching an already-detached record: %v", err)
		}
	})

	t.Run("the automation no longer touches it", func(t *testing.T) {
		rec := f.recordAt("www.example.com.", zone.TypeA, "192.0.2.10")

		// D4, knowingly: a detached entry survives the record it came from.
		// Deleting authored data as a side effect of an unrelated change is
		// worse than leaving something the consistency check will flag.
		if _, err := f.a.Apply(t.Context(), f.command(apply.RecordOp{
			Action: apply.ActionDelete, RecordID: rec.ID,
		})); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if got := f.ptrs(rev); !slices.Equal(got, []string{"10.2.0.192.in-addr.arpa. -> www.example.com."}) {
			t.Errorf("the detached entry did not survive: %v", got)
		}
	})

	t.Run("a detached entry is never taken away, whatever the policy", func(t *testing.T) {
		f := newFixtureWith(t, apply.Options{Policy: apply.PolicyLastWins})
		rev := f.reverseZone("2.0.192.in-addr.arpa.")

		f.addA("first.example.com.", "192.0.2.10")
		page, err := f.s.ListRecords(t.Context(), store.RecordFilter{
			ZoneID: rev.ID, Types: []zone.RRType{zone.TypePTR},
		})
		if err != nil || len(page.Items) != 1 {
			t.Fatalf("ListRecords: %v", err)
		}
		if _, derr := f.a.Apply(t.Context(), apply.Command{
			ZoneID: rev.ID, Kind: journal.KindEdit, Source: journal.SourceAPI,
			Ops: []apply.RecordOp{{Action: apply.ActionDetach, RecordID: page.Items[0].ID}},
		}); derr != nil {
			t.Fatalf("detaching: %v", derr)
		}

		res, err := f.a.Apply(t.Context(), f.command(apply.RecordOp{
			Action: apply.ActionAdd, Record: f.record("second.example.com.", zone.TypeA, 3600, "192.0.2.10"),
		}))
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if got := f.ptrs(rev); !slices.Equal(got, []string{"10.2.0.192.in-addr.arpa. -> first.example.com."}) {
			t.Errorf("last-wins took over a detached entry: %v", got)
		}
		if len(res.Conflicts) != 1 || res.Conflicts[0].Generated {
			t.Errorf("the conflict does not say the entry was authored: %+v", res.Conflicts)
		}
	})
}

// An applier that was never told either way generates reverse entries. Nil is
// on, not off: every other test in this file leans on that through the
// fixture, so state it once on its own.
func TestReverseDefaultsToOn(t *testing.T) {
	t.Parallel()
	f := newFixtureWith(t, apply.Options{})
	rev := f.reverseZone("2.0.192.in-addr.arpa.")

	f.addA("www.example.com.", "192.0.2.10")
	if got := f.ptrs(rev); !slices.Equal(got, []string{"10.2.0.192.in-addr.arpa. -> www.example.com."}) {
		t.Errorf("the reverse zone holds %v", got)
	}
}

func TestReverseCanBeTurnedOff(t *testing.T) {
	t.Parallel()

	t.Run("off for the server", func(t *testing.T) {
		t.Parallel()
		f := newFixtureWith(t, apply.Options{AutoReverse: ref(false)})
		rev := f.reverseZone("2.0.192.in-addr.arpa.")

		res, err := f.a.Apply(t.Context(), f.command(apply.RecordOp{
			Action: apply.ActionAdd, Record: f.record("www.example.com.", zone.TypeA, 3600, "192.0.2.10"),
		}))
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if got := f.ptrs(rev); len(got) != 0 {
			t.Errorf("entries were generated with the automation off: %v", got)
		}
		if len(res.MissingZones) != 0 {
			t.Errorf("the automation is off, so there is nothing to suggest: %v", res.MissingZones)
		}
	})

	t.Run("off for one zone", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		rev := f.reverseZone("2.0.192.in-addr.arpa.")

		off := false
		next := *f.z
		next.AutoReverse = &off
		if _, err := f.a.UpdateZone(t.Context(), &next, testMeta()); err != nil {
			t.Fatalf("UpdateZone: %v", err)
		}

		f.addA("www.example.com.", "192.0.2.10")
		if got := f.ptrs(rev); len(got) != 0 {
			t.Errorf("entries were generated for a zone that has it off: %v", got)
		}
	})
}

// Creating a zone generates the entries its records imply, in the same
// transaction and as the reverse zone's own commit.
func TestReverseOnZoneCreate(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rev := f.reverseZone("2.0.192.in-addr.arpa.")

	z := newZone(t, "example.net.")
	records := []zone.Record{apexNS(t, z, "ns1.example.net.")}
	www, err := zone.NewRecord(z.ID, zone.MustParseName("www.example.net."),
		zone.ClassIN, zone.TypeA, 3600, "192.0.2.50")
	if err != nil {
		t.Fatalf("NewRecord: %v", err)
	}
	records = append(records, www)

	res, err := f.a.CreateZone(t.Context(), z, records, testMeta())
	if err != nil {
		t.Fatalf("CreateZone: %v", err)
	}
	if len(res.Commits) != 2 {
		t.Fatalf("creating the zone produced %d commits, want two", len(res.Commits))
	}
	if res.Commits[0].Kind != journal.KindZoneCreate {
		t.Errorf("the first commit is a %q", res.Commits[0].Kind)
	}
	if got := f.ptrs(rev); !slices.Equal(got, []string{"50.2.0.192.in-addr.arpa. -> www.example.net."}) {
		t.Errorf("the reverse zone holds %v", got)
	}
}

// Deleting a zone takes the entries its records generated elsewhere with it —
// and the zones holding them record that it happened.
func TestReverseOnZoneDelete(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rev := f.reverseZone("2.0.192.in-addr.arpa.")

	f.addA("www.example.com.", "192.0.2.10")
	f.addA("mail.example.com.", "192.0.2.11")
	if got := f.ptrs(rev); len(got) != 2 {
		t.Fatalf("the reverse zone holds %v, want two entries", got)
	}
	revSerial := func() zone.Serial {
		z, err := f.s.ZoneByID(t.Context(), rev.ID)
		if err != nil {
			t.Fatalf("ZoneByID: %v", err)
		}
		return z.SOA.Serial
	}
	before := revSerial()

	res, err := f.a.DeleteZone(t.Context(), f.z.ID, testMeta())
	if err != nil {
		t.Fatalf("DeleteZone: %v", err)
	}

	if got := f.ptrs(rev); len(got) != 0 {
		t.Errorf("the reverse zone still holds %v", got)
	}
	if len(res.Commits) != 2 {
		t.Fatalf("the deletion produced %d commits, want two", len(res.Commits))
	}
	if res.Commits[0].Kind != journal.KindZoneDelete {
		t.Errorf("the first commit is a %q, want the deletion", res.Commits[0].Kind)
	}
	// Both removals in one commit, so the reverse zone advances by one step for
	// one cause.
	if got := len(res.Commits[1].Events); got != 2 {
		t.Errorf("the reverse commit carries %d events, want two", got)
	}
	if got := revSerial(); got != before.Next() {
		t.Errorf("the reverse zone went from serial %s to %s, want one step", before, got)
	}
}

// records renders one zone's records of a given type as "owner -> data" lines.
func (f *fixture) recordsOfType(z *zone.Zone, typ zone.RRType) []string {
	f.t.Helper()

	var out []string
	for r, err := range f.s.IterZoneRecords(f.t.Context(), z.ID) {
		if err != nil {
			f.t.Fatalf("IterZoneRecords: %v", err)
		}
		if r.Type == typ {
			out = append(out, r.Name.String()+" -> "+r.RData.String())
		}
	}
	return out
}

// RFC 2317 §4: a resolver asked about 192.0.2.10 looks up
// "10.2.0.192.in-addr.arpa.", which lives in the /24 rather than in the
// classless child that answers for the address. The /24 therefore has to carry
// a CNAME pointing into the child, and writing one per address by hand is the
// tedious half of RFC 2317 (D7).
func TestReverseClasslessDelegation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	parent := f.reverseZone("2.0.192.in-addr.arpa.")
	child := f.reverseZone("0/25.2.0.192.in-addr.arpa.")

	res, err := f.a.Apply(t.Context(), f.command(apply.RecordOp{
		Action: apply.ActionAdd, Record: f.record("www.example.com.", zone.TypeA, 3600, "192.0.2.10"),
	}))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// The forward zone, the child that answers, and the parent that points at
	// it: three zones, three commits.
	if len(res.Commits) != 3 {
		t.Fatalf("the change produced %d commits, want three", len(res.Commits))
	}

	if got := f.ptrs(child); !slices.Equal(got,
		[]string{"10.0/25.2.0.192.in-addr.arpa. -> www.example.com."}) {
		t.Errorf("the classless child holds %v", got)
	}
	if got := f.recordsOfType(parent, zone.TypeCNAME); !slices.Equal(got,
		[]string{"10.2.0.192.in-addr.arpa. -> 10.0/25.2.0.192.in-addr.arpa."}) {
		t.Errorf("the parent holds %v", got)
	}

	t.Run("the delegation says what it is", func(t *testing.T) {
		page, perr := f.s.ListRecords(t.Context(), store.RecordFilter{
			ZoneID: parent.ID, Types: []zone.RRType{zone.TypeCNAME},
		})
		if perr != nil || len(page.Items) != 1 {
			t.Fatalf("ListRecords: %v", perr)
		}
		if page.Items[0].ManagedKind != zone.ManagedRFC2317CNAME {
			t.Errorf("the delegation is marked %q", page.Items[0].ManagedKind)
		}
	})

	t.Run("re-applying changes nothing", func(t *testing.T) {
		again, aerr := f.a.Apply(t.Context(), f.command(apply.RecordOp{
			Action:  apply.ActionReplaceRRset,
			Key:     zone.RRsetKey{Name: zone.MustParseName("www.example.com."), Class: zone.ClassIN, Type: zone.TypeA},
			Records: []zone.Record{*f.record("www.example.com.", zone.TypeA, 3600, "192.0.2.10")},
		}))
		if aerr != nil {
			t.Fatalf("Apply: %v", aerr)
		}
		if again.Changed() {
			t.Errorf("re-applying produced %d commits", len(again.Commits))
		}
	})

	// The chain is two links long (the delegation hangs off the entry, which
	// hangs off the address record) and both have to come away, each recorded
	// in the journal of the zone it was in.
	t.Run("deleting the source unwinds the whole chain", func(t *testing.T) {
		rec := f.recordAt("www.example.com.", zone.TypeA, "192.0.2.10")

		del, derr := f.a.Apply(t.Context(), f.command(apply.RecordOp{
			Action: apply.ActionDelete, RecordID: rec.ID,
		}))
		if derr != nil {
			t.Fatalf("Apply: %v", derr)
		}
		if got := f.ptrs(child); len(got) != 0 {
			t.Errorf("the child still holds %v", got)
		}
		if got := f.recordsOfType(parent, zone.TypeCNAME); len(got) != 0 {
			t.Errorf("the parent still holds %v", got)
		}
		if len(del.Commits) != 3 {
			t.Errorf("the deletion produced %d commits, want three", len(del.Commits))
		}
	})
}

func TestReverseDelegationOnlyWhereWeHoldTheParent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	// The classless child on its own: the /24 belongs to someone else, and
	// records in someone else's zone are not ours to write (D7).
	child := f.reverseZone("0/25.2.0.192.in-addr.arpa.")

	res, err := f.a.Apply(t.Context(), f.command(apply.RecordOp{
		Action: apply.ActionAdd, Record: f.record("www.example.com.", zone.TypeA, 3600, "192.0.2.10"),
	}))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.Commits) != 2 {
		t.Errorf("the change reached %d zones, want two", len(res.Commits))
	}
	if got := f.ptrs(child); len(got) != 1 {
		t.Errorf("the child holds %v, want the entry", got)
	}
}

// RFC 2181 §10.1: where a CNAME is, nothing else may be. Something already
// answering in the parent is not this automation's to take away.
func TestReverseDelegationYieldsToWhatIsThere(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	parent := f.reverseZone("2.0.192.in-addr.arpa.")
	f.reverseZone("0/25.2.0.192.in-addr.arpa.")

	// An authored PTR in the parent, at the name the delegation would take.
	held, err := zone.NewRecord(parent.ID, zone.MustParseName("10.2.0.192.in-addr.arpa."),
		zone.ClassIN, zone.TypePTR, 3600, "legacy.example.com.")
	if err != nil {
		t.Fatalf("NewRecord: %v", err)
	}
	if _, aerr := f.a.Apply(t.Context(), apply.Command{
		ZoneID: parent.ID, Kind: journal.KindEdit, Source: journal.SourceAPI,
		Ops: []apply.RecordOp{{Action: apply.ActionAdd, Record: &held}},
	}); aerr != nil {
		t.Fatalf("Apply: %v", aerr)
	}

	res, err := f.a.Apply(t.Context(), f.command(apply.RecordOp{
		Action: apply.ActionAdd, Record: f.record("www.example.com.", zone.TypeA, 3600, "192.0.2.10"),
	}))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got := f.recordsOfType(parent, zone.TypeCNAME); len(got) != 0 {
		t.Errorf("a delegation was written over what was already there: %v", got)
	}
	if got := f.recordsOfType(parent, zone.TypePTR); !slices.Equal(got,
		[]string{"10.2.0.192.in-addr.arpa. -> legacy.example.com."}) {
		t.Errorf("the parent holds %v", got)
	}
	if len(res.Conflicts) != 1 {
		t.Fatalf("the change reported %d conflicts, want one", len(res.Conflicts))
	}
	if res.Conflicts[0].ReverseZone != parent.ID {
		t.Errorf("the conflict names the wrong zone: %+v", res.Conflicts[0])
	}
}

// Reverse automation reacts to changes, so a zone that arrives after the
// records has no change to react to. Creating it and then filling it is the
// answer; D6 forbids only creating it unasked.
func TestReconcile(t *testing.T) {
	t.Parallel()

	t.Run("a reverse zone fills from the records that were already there", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)

		// No reverse zone yet, so these are only offered.
		res, err := f.a.Apply(t.Context(), f.command(
			apply.RecordOp{Action: apply.ActionAdd, Record: f.record("www.example.com.", zone.TypeA, 3600, "192.0.2.10")},
			apply.RecordOp{Action: apply.ActionAdd, Record: f.record("mail.example.com.", zone.TypeA, 3600, "192.0.2.11")},
			apply.RecordOp{Action: apply.ActionAdd, Record: f.record("far.example.com.", zone.TypeA, 3600, "198.51.100.1")},
		))
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if len(res.MissingZones) != 3 {
			t.Fatalf("the change reported %d missing zones, want three", len(res.MissingZones))
		}

		// Creating the zone fills it, because a zone arriving after the records
		// it should hold has no change to react to and nothing else would ever
		// write them (D21).
		rev := f.reverseZone("2.0.192.in-addr.arpa.")

		want := []string{
			"10.2.0.192.in-addr.arpa. -> www.example.com.",
			"11.2.0.192.in-addr.arpa. -> mail.example.com.",
		}
		if entries := f.ptrs(rev); !slices.Equal(entries, want) {
			t.Errorf("the reverse zone holds\n  got  %v\n  want %v", entries, want)
		}

		t.Run("running it again changes nothing", func(t *testing.T) {
			again, aerr := f.a.Reconcile(t.Context(), rev.ID, testMeta())
			if aerr != nil {
				t.Fatalf("Reconcile: %v", aerr)
			}
			if again.Changed() {
				t.Errorf("a second reconciliation produced %d commits", len(again.Commits))
			}
		})
	})

	t.Run("a forward zone fills its own records' entries", func(t *testing.T) {
		t.Parallel()
		f := newFixtureWith(t, apply.Options{AutoReverse: ref(false)})
		rev := f.reverseZone("2.0.192.in-addr.arpa.")
		f.addA("www.example.com.", "192.0.2.10")

		if got := f.ptrs(rev); len(got) != 0 {
			t.Fatalf("entries were generated with the automation off: %v", got)
		}

		// Turn it on for this zone, then catch up.
		on := true
		next := *f.z
		next.AutoReverse = &on
		if _, err := f.a.UpdateZone(t.Context(), &next, testMeta()); err != nil {
			t.Fatalf("UpdateZone: %v", err)
		}

		if _, err := f.a.Reconcile(t.Context(), f.z.ID, testMeta()); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if got := f.ptrs(rev); !slices.Equal(got, []string{"10.2.0.192.in-addr.arpa. -> www.example.com."}) {
			t.Errorf("the reverse zone holds %v", got)
		}
	})

	t.Run("a zone with the automation off is not filled from the other side", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)

		off := false
		next := *f.z
		next.AutoReverse = &off
		if _, err := f.a.UpdateZone(t.Context(), &next, testMeta()); err != nil {
			t.Fatalf("UpdateZone: %v", err)
		}
		f.addA("www.example.com.", "192.0.2.10")

		rev := f.reverseZone("2.0.192.in-addr.arpa.")
		if _, err := f.a.Reconcile(t.Context(), rev.ID, testMeta()); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if got := f.ptrs(rev); len(got) != 0 {
			t.Errorf("the reverse zone was filled anyway: %v", got)
		}
	})

	t.Run("a detached entry is left alone", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		rev := f.reverseZone("2.0.192.in-addr.arpa.")
		f.addA("www.example.com.", "192.0.2.10")

		page, err := f.s.ListRecords(t.Context(), store.RecordFilter{
			ZoneID: rev.ID, Types: []zone.RRType{zone.TypePTR},
		})
		if err != nil || len(page.Items) != 1 {
			t.Fatalf("ListRecords: %v", err)
		}
		if _, derr := f.a.Apply(t.Context(), apply.Command{
			ZoneID: rev.ID, Kind: journal.KindEdit, Source: journal.SourceAPI,
			Ops: []apply.RecordOp{{Action: apply.ActionDetach, RecordID: page.Items[0].ID}},
		}); derr != nil {
			t.Fatalf("detaching: %v", derr)
		}

		got, err := f.a.Reconcile(t.Context(), rev.ID, testMeta())
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		// The entry is already the right answer, so there is nothing to add and
		// nothing to take away.
		if got.Changed() {
			t.Errorf("the reconciliation touched a detached entry: %d commits", len(got.Commits))
		}
	})

	t.Run("the classless child is filled with its delegations", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		parent := f.reverseZone("2.0.192.in-addr.arpa.")
		f.addA("www.example.com.", "192.0.2.10")

		child := f.reverseZone("0/25.2.0.192.in-addr.arpa.")
		if _, err := f.a.Reconcile(t.Context(), child.ID, testMeta()); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}

		if got := f.ptrs(child); !slices.Equal(got,
			[]string{"10.0/25.2.0.192.in-addr.arpa. -> www.example.com."}) {
			t.Errorf("the child holds %v", got)
		}
		if got := f.recordsOfType(parent, zone.TypeCNAME); !slices.Equal(got,
			[]string{"10.2.0.192.in-addr.arpa. -> 10.0/25.2.0.192.in-addr.arpa."}) {
			t.Errorf("the parent holds %v", got)
		}
	})
}

// The two moments reverse automation has nothing to react to, and what D21
// asks to happen at each.
func TestAutomationFillsWhatItHadNoChangeToReactTo(t *testing.T) {
	t.Parallel()

	t.Run("a reverse zone arriving after the addresses", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.addA("www.example.com.", "192.0.2.10")

		rev := f.reverseZone("2.0.192.in-addr.arpa.")

		want := []string{"10.2.0.192.in-addr.arpa. -> www.example.com."}
		if got := f.ptrs(rev); !slices.Equal(got, want) {
			t.Errorf("the new zone holds\n  got  %v\n  want %v", got, want)
		}
	})

	t.Run("automation switched on for a zone that already has records", func(t *testing.T) {
		t.Parallel()
		f := newFixtureWith(t, apply.Options{AutoReverse: ptrTo(false)})
		rev := f.reverseZone("2.0.192.in-addr.arpa.")
		f.addA("www.example.com.", "192.0.2.10")

		if got := f.ptrs(rev); len(got) != 0 {
			t.Fatalf("automation is off and something was generated anyway: %v", got)
		}

		next := *f.z
		next.AutoReverse = ptrTo(true)
		if _, err := f.a.UpdateZone(t.Context(), &next, testMeta()); err != nil {
			t.Fatalf("UpdateZone: %v", err)
		}

		want := []string{"10.2.0.192.in-addr.arpa. -> www.example.com."}
		if got := f.ptrs(rev); !slices.Equal(got, want) {
			t.Errorf("switching automation on left\n  got  %v\n  want %v", got, want)
		}
	})

	// Off does not undo. What may be taken away is the question D4 leaves to
	// the person, and reverse automation only ever adds.
	t.Run("switching it off again leaves what was written", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		rev := f.reverseZone("2.0.192.in-addr.arpa.")
		f.addA("www.example.com.", "192.0.2.10")

		next := *f.z
		next.AutoReverse = ptrTo(false)
		if _, err := f.a.UpdateZone(t.Context(), &next, testMeta()); err != nil {
			t.Fatalf("UpdateZone: %v", err)
		}

		if got := f.ptrs(rev); len(got) != 1 {
			t.Errorf("switching automation off removed entries: %v", got)
		}
	})
}

// ptrTo is a pointer to a value, for the three-state settings where nil means
// "follow the server".
func ptrTo[T any](v T) *T { return &v }

// D3 asks for an answer to a conflict and not only a report of one: the losing
// name takes the entry, and the check goes quiet because the state it named is
// no longer true (D33).
func TestMakeCanonicalTakesTheEntry(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	rev := f.reverseZone("2.0.192.in-addr.arpa.")
	f.addA("www.example.com.", "192.0.2.10")
	f.addA("mail.example.com.", "192.0.2.10")

	if got := f.ptrs(rev); !slices.Equal(got, []string{
		"10.2.0.192.in-addr.arpa. -> www.example.com.",
	}) {
		t.Fatalf("first-wins did not hold: %v", got)
	}

	f.mustApply(f.command(apply.RecordOp{
		Action:   apply.ActionMakeCanonical,
		RecordID: f.recordID("mail.example.com.", zone.TypeA),
	}))

	want := []string{"10.2.0.192.in-addr.arpa. -> mail.example.com."}
	if got := f.ptrs(rev); !slices.Equal(got, want) {
		t.Errorf("the reverse zone holds\n  got  %v\n  want %v", got, want)
	}

	// The conflict does not go away, and should not: two names still claim one
	// address. What changed is which of them the reverse answers with, and the
	// check now says so from the other side.
	found, err := f.a.CheckReverse(t.Context(), rev.ID)
	if err != nil {
		t.Fatalf("CheckReverse: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("got %d findings, want the same conflict from the other side: %+v", len(found), found)
	}
	if !strings.Contains(found[0].Detail, "as mail.example.com.") ||
		!strings.Contains(found[0].Detail, "www.example.com. points at it") {
		t.Errorf("detail is %q, want mail answering and www without an entry", found[0].Detail)
	}
}

// A hand-written entry is never taken away, whoever asks. Detaching one is how
// a person says to leave it alone (D4), and this must not undo that.
func TestMakeCanonicalLeavesAHandWrittenEntry(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	rev := f.reverseZone("2.0.192.in-addr.arpa.")
	f.addA("www.example.com.", "192.0.2.10")

	// Detached, so the entry is somebody's own rather than the server's.
	ptr := f.reversePTR(rev, "10.2.0.192.in-addr.arpa.")
	f.mustApply(apply.Command{
		ZoneID: rev.ID, Kind: journal.KindEdit, Source: journal.SourceAPI, Actor: "test",
		Ops: []apply.RecordOp{{Action: apply.ActionDetach, RecordID: ptr}},
	})

	f.addA("mail.example.com.", "192.0.2.10")
	f.mustApply(f.command(apply.RecordOp{
		Action:   apply.ActionMakeCanonical,
		RecordID: f.recordID("mail.example.com.", zone.TypeA),
	}))

	want := []string{"10.2.0.192.in-addr.arpa. -> www.example.com."}
	if got := f.ptrs(rev); !slices.Equal(got, want) {
		t.Errorf("a detached entry was taken away\n  got  %v\n  want %v", got, want)
	}
}

// reversePTR finds the identifier of one PTR in a reverse zone.
func (f *fixture) reversePTR(rev *zone.Zone, name string) zone.RecordID {
	f.t.Helper()

	for rec, err := range f.s.IterZoneRecords(f.t.Context(), rev.ID) {
		if err != nil {
			f.t.Fatalf("IterZoneRecords: %v", err)
		}
		if rec.Type == zone.TypePTR && rec.Name.String() == name {
			return rec.ID
		}
	}
	f.t.Fatalf("no PTR at %s", name)
	return ""
}
