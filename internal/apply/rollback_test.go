package apply_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/apply"
	"github.com/wegweiserzone/wegweiser/internal/journal"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

func TestRollbackRestoresRecords(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	f.mustApply(f.command(apply.RecordOp{
		Action: apply.ActionAdd,
		Record: f.record("www.example.com.", zone.TypeA, 300, "192.0.2.10"),
	}))
	target := f.serial()
	want := f.records()

	// Three changes on top: an addition, a removal and an edit.
	f.mustApply(f.command(apply.RecordOp{
		Action: apply.ActionAdd,
		Record: f.record("mail.example.com.", zone.TypeA, 300, "192.0.2.20"),
	}))
	www := f.recordAt("www.example.com.", zone.TypeA, "192.0.2.10")
	f.mustApply(f.command(apply.RecordOp{
		Action: apply.ActionUpdate, RecordID: www.ID,
		Record: f.record("www.example.com.", zone.TypeA, 300, "192.0.2.11"),
	}))
	mail := f.recordAt("mail.example.com.", zone.TypeA, "192.0.2.20")
	f.mustApply(f.command(apply.RecordOp{Action: apply.ActionDelete, RecordID: mail.ID}))

	res, err := f.a.Rollback(f.t.Context(), f.z.ID, target, testMeta())
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if got := f.records(); !slices.Equal(got, want) {
		t.Errorf("the zone holds %v, want the state at serial %s: %v", got, target, want)
	}

	t.Run("it moves forward, it does not rewind", func(t *testing.T) {
		// A secondary that has already seen a later serial will never accept a
		// jump back to an earlier one: RFC 1982 makes it older and RFC 1995
		// cannot express going backwards. See data model §3.7.
		c := res.Commit()
		if !c.SerialFrom.Before(c.SerialTo) {
			t.Errorf("serial went %s → %s, want it to move on", c.SerialFrom, c.SerialTo)
		}
		if c.Kind != journal.KindRollback {
			t.Errorf("kind = %q, want a rollback", c.Kind)
		}
		if c.RevertsTo == nil || *c.RevertsTo != target {
			t.Errorf("revertsTo = %v, want %s", c.RevertsTo, target)
		}
	})

	t.Run("the record it restored has a new identity", func(t *testing.T) {
		// The row was deleted; putting the same data back is a new record, and
		// pretending otherwise would mean holding onto rows that are gone.
		if got := f.recordAt("www.example.com.", zone.TypeA, "192.0.2.10"); got.ID == www.ID {
			t.Error("the restored record reused the identity of the one that was removed")
		}
	})

	t.Run("the history goes forwards only", func(t *testing.T) {
		page, perr := f.s.ListCommits(f.t.Context(), store.CommitFilter{ZoneID: f.z.ID})
		if perr != nil {
			t.Fatalf("ListCommits: %v", perr)
		}
		if page.Items[0].ID != res.Commit().ID {
			t.Error("the rollback is not the newest commit")
		}
		// The zone's creation, the record that was added before the target,
		// the three changes after it, and the rollback.
		if len(page.Items) != 6 {
			t.Errorf("history holds %d commits, want nothing removed and one added",
				len(page.Items))
		}
	})
}

func TestRollbackLeavesUntouchedRecordsAlone(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	f.mustApply(f.command(apply.RecordOp{
		Action: apply.ActionAdd,
		Record: f.record("keep.example.com.", zone.TypeA, 300, "192.0.2.1"),
	}))
	keep := f.recordAt("keep.example.com.", zone.TypeA, "192.0.2.1")
	target := f.serial()

	f.mustApply(f.command(apply.RecordOp{
		Action: apply.ActionAdd,
		Record: f.record("gone.example.com.", zone.TypeA, 300, "192.0.2.2"),
	}))

	if _, err := f.a.Rollback(f.t.Context(), f.z.ID, target, testMeta()); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	// A rollback that rewrote every record would lose the comment, the
	// provenance and the history pointing at the ones it did not have to touch.
	if got := f.recordAt("keep.example.com.", zone.TypeA, "192.0.2.1"); got.ID != keep.ID {
		t.Errorf("the untouched record is now %q, was %q", got.ID, keep.ID)
	}
}

func TestRollbackRestoresATTL(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	f.mustApply(f.command(apply.RecordOp{
		Action: apply.ActionAdd,
		Record: f.record("www.example.com.", zone.TypeA, 300, "192.0.2.10"),
	}))
	target := f.serial()
	www := f.recordAt("www.example.com.", zone.TypeA, "192.0.2.10")

	f.mustApply(f.command(apply.RecordOp{
		Action: apply.ActionUpdate, RecordID: www.ID,
		Record: f.record("www.example.com.", zone.TypeA, 60, "192.0.2.10"),
	}))

	if _, err := f.a.Rollback(f.t.Context(), f.z.ID, target, testMeta()); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	got := f.recordAt("www.example.com.", zone.TypeA, "192.0.2.10")
	if got.TTL != 300 {
		t.Errorf("ttl = %d, want the 300 it had at the target", got.TTL)
	}
	// The data never changed, so the record never went away and kept its row.
	if got.ID != www.ID {
		t.Errorf("the record is now %q, was %q; only its lifetime changed", got.ID, www.ID)
	}
}

func TestRollbackRestoresTheStartOfAuthority(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	target := f.serial()

	z, err := f.s.ZoneByID(f.t.Context(), f.z.ID)
	if err != nil {
		t.Fatalf("ZoneByID: %v", err)
	}
	wasRefresh := z.SOA.Refresh
	z.SOA.Refresh = 7200
	z.Comment = "edited"
	if _, uerr := f.a.UpdateZone(f.t.Context(), z, testMeta()); uerr != nil {
		t.Fatalf("UpdateZone: %v", uerr)
	}

	res, err := f.a.Rollback(f.t.Context(), f.z.ID, target, testMeta())
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	after, err := f.s.ZoneByID(f.t.Context(), f.z.ID)
	if err != nil {
		t.Fatalf("ZoneByID: %v", err)
	}
	if after.SOA.Refresh != wasRefresh {
		t.Errorf("refresh = %d, want the %d it had at the target", after.SOA.Refresh, wasRefresh)
	}
	if after.SOA.Serial != res.Commit().SerialTo {
		t.Errorf("serial = %s, want the one the rollback wrote (%s)",
			after.SOA.Serial, res.Commit().SerialTo)
	}

	t.Run("the change is in the history as a change to the SOA", func(t *testing.T) {
		lines := eventLines(res.Commit())
		if len(lines) != 2 {
			t.Fatalf("events = %v, want the SOA leaving and arriving", lines)
		}
		if lines[0][0] != '-' || lines[1][0] != '+' {
			t.Errorf("events = %v, want a removal then an addition", lines)
		}
	})

	t.Run("what is not served is not restored", func(t *testing.T) {
		// The comment is not in the journal, because it is not a record. A
		// rollback restores the zone as it answers, not as it was annotated.
		if after.Comment != "edited" {
			t.Errorf("comment = %q, want the edit left alone", after.Comment)
		}
	})
}

func TestRollbackTakesTheReverseZoneWithIt(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	rev := newZone(t, "2.0.192.in-addr.arpa.")
	ns, err := zone.NewRecord(rev.ID, rev.Name, zone.ClassIN, zone.TypeNS, 3600, "ns1.example.com.")
	if err != nil {
		t.Fatalf("NewRecord: %v", err)
	}
	if _, cerr := f.a.CreateZone(f.t.Context(), rev, []zone.Record{ns}, testMeta()); cerr != nil {
		t.Fatalf("CreateZone: %v", cerr)
	}

	target := f.serial()
	f.mustApply(f.command(apply.RecordOp{
		Action: apply.ActionAdd,
		Record: f.record("www.example.com.", zone.TypeA, 300, "192.0.2.10"),
	}))

	res, err := f.a.Rollback(f.t.Context(), f.z.ID, target, testMeta())
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	// The PTR is derived, so nothing restores it directly: taking the address
	// record away is what takes it away.
	page, err := f.s.ListRecords(f.t.Context(), store.RecordFilter{
		ZoneID: rev.ID, Types: []zone.RRType{zone.TypePTR},
	})
	if err != nil {
		t.Fatalf("ListRecords: %v", err)
	}
	if len(page.Items) != 0 {
		t.Errorf("the reverse zone still holds %d PTRs", len(page.Items))
	}
	if len(res.Commits) != 2 {
		t.Errorf("the rollback wrote %d commits, want one per zone it reached", len(res.Commits))
	}
}

func TestRollbackRefusals(t *testing.T) {
	t.Parallel()

	t.Run("to the serial it is already at", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)

		res, err := f.a.Rollback(f.t.Context(), f.z.ID, f.serial(), testMeta())
		if err != nil {
			t.Fatalf("Rollback: %v", err)
		}
		if res.Changed() {
			t.Error("a rollback to where the zone already is wrote a commit")
		}
	})

	t.Run("to a serial the zone has not reached", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)

		_, err := f.a.Rollback(f.t.Context(), f.z.ID, f.serial().Next(), testMeta())
		if err == nil {
			t.Fatal("rolled forward into a state the zone has never had")
		}
		if !errors.Is(err, zone.ErrInvalid) {
			t.Errorf("error = %v, want it to wrap ErrInvalid", err)
		}
	})

	t.Run("to a serial that is not in the history", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)

		// Older than anything recorded: the zone's first commit starts at the
		// serial a new zone is created with, and nothing leads to this one.
		_, err := f.a.Rollback(f.t.Context(), f.z.ID, zone.NewSerial(1<<31), testMeta())
		if err == nil {
			t.Fatal("restored a state that was never recorded")
		}
		if !errors.Is(err, store.ErrNotFound) && !errors.Is(err, zone.ErrInvalid) {
			t.Errorf("error = %v, want it to say the state is not there", err)
		}
	})

	t.Run("of a zone that is not there", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)

		_, err := f.a.Rollback(f.t.Context(), zone.ZoneID("01ARZ3NDEKTSV4RRFFQ69G5FAV"),
			zone.NewSerial(1), testMeta())
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("error = %v, want ErrNotFound", err)
		}
	})

	t.Run("that would leave the zone unusable", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)

		// The zone was created with one apex NS. Adding a second and rolling
		// back to before it is fine; the state being restored is a valid zone,
		// which is what the check is there to establish.
		f.mustApply(f.command(apply.RecordOp{
			Action: apply.ActionAdd,
			Record: f.record("example.com.", zone.TypeNS, 3600, "ns2.example.com."),
		}))
		target := f.serial()
		f.mustApply(f.command(apply.RecordOp{
			Action: apply.ActionAdd,
			Record: f.record("extra.example.com.", zone.TypeA, 300, "192.0.2.1"),
		}))

		if _, err := f.a.Rollback(f.t.Context(), f.z.ID, target, testMeta()); err != nil {
			t.Fatalf("Rollback: %v", err)
		}
		if got := f.recordAt("example.com.", zone.TypeNS, "ns2.example.com."); got == nil {
			t.Error("the apex NS added before the target was taken away")
		}
	})
}

// TestRollbackAcrossRecordsAndTheSOA is the case that a rollback touching only
// one of the two never exercises: a commit is a difference sequence, so every
// deletion comes before every addition (RFC 1995 §2), and the SOA has to take
// its place in that order rather than sit in front of it.
func TestRollbackAcrossRecordsAndTheSOA(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	f.mustApply(f.command(apply.RecordOp{
		Action: apply.ActionAdd,
		Record: f.record("www.example.com.", zone.TypeA, 300, "192.0.2.10"),
	}))
	target := f.serial()
	want := f.records()

	z, err := f.s.ZoneByID(f.t.Context(), f.z.ID)
	if err != nil {
		t.Fatalf("ZoneByID: %v", err)
	}
	wasRefresh := z.SOA.Refresh
	z.SOA.Refresh = 7200
	if _, uerr := f.a.UpdateZone(f.t.Context(), z, testMeta()); uerr != nil {
		t.Fatalf("UpdateZone: %v", uerr)
	}
	www := f.recordAt("www.example.com.", zone.TypeA, "192.0.2.10")
	f.mustApply(f.command(apply.RecordOp{
		Action: apply.ActionUpdate, RecordID: www.ID,
		Record: f.record("www.example.com.", zone.TypeA, 60, "192.0.2.99"),
	}))

	res, err := f.a.Rollback(f.t.Context(), f.z.ID, target, testMeta())
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if got := f.records(); !slices.Equal(got, want) {
		t.Errorf("the zone holds %v, want %v", got, want)
	}
	after, err := f.s.ZoneByID(f.t.Context(), f.z.ID)
	if err != nil {
		t.Fatalf("ZoneByID: %v", err)
	}
	if after.SOA.Refresh != wasRefresh {
		t.Errorf("refresh = %d, want the %d it had at the target", after.SOA.Refresh, wasRefresh)
	}

	lines := eventLines(res.Commit())
	seenAdd := false
	for _, l := range lines {
		if l[0] == '+' {
			seenAdd = true
		} else if seenAdd {
			t.Fatalf("events = %v, want every deletion before every addition", lines)
		}
	}
	if lines[0][0] != '-' || !slices.ContainsFunc(lines, func(l string) bool {
		return l[0] == '-' && strings.Contains(l, "SOA")
	}) {
		t.Errorf("events = %v, want the SOA leading the deletions", lines)
	}
}
