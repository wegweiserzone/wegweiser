package apply_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/apply"
	"github.com/wegweiserzone/wegweiser/internal/id"
	"github.com/wegweiserzone/wegweiser/internal/journal"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

func apexNS(t *testing.T, z *zone.Zone, target string) zone.Record {
	t.Helper()

	r, err := zone.NewRecord(z.ID, z.Name, zone.ClassIN, zone.TypeNS, 3600, target)
	if err != nil {
		t.Fatalf("NewRecord: %v", err)
	}
	return r
}

func TestCreateZone(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	t.Run("the zone and its first records arrive as one commit", func(t *testing.T) {
		z := newZone(t, "example.net.")
		z.SOA.Serial = zone.NewSerial(2026081601) // as an import would carry it

		www, err := zone.NewRecord(z.ID, zone.MustParseName("www.example.net."),
			zone.ClassIN, zone.TypeA, 3600, "192.0.2.10")
		if err != nil {
			t.Fatalf("NewRecord: %v", err)
		}

		records := []zone.Record{apexNS(t, z, "ns1.example.net."), www}
		res, err := f.a.CreateZone(t.Context(), z, records,
			apply.Meta{Source: journal.SourceImport, Actor: "migration", Comment: "moved off BIND"})
		if err != nil {
			t.Fatalf("CreateZone: %v", err)
		}
		c := res.Commit()

		if c.Kind != journal.KindZoneCreate {
			t.Errorf("Kind = %q, want %q", c.Kind, journal.KindZoneCreate)
		}
		// A zone that did not exist has no serial to step from, and an import
		// keeps the serial its secondaries have already seen (D2).
		if !c.SerialFrom.IsZero() {
			t.Errorf("SerialFrom = %s, want 0", c.SerialFrom)
		}
		if want := zone.NewSerial(2026081601); c.SerialTo != want {
			t.Errorf("SerialTo = %s, want %s", c.SerialTo, want)
		}
		if !c.ZoneName.Equal(z.Name) {
			t.Errorf("ZoneName = %q, want %q", c.ZoneName, z.Name)
		}
		if verr := c.Validate(); verr != nil {
			t.Errorf("the commit does not validate: %v", verr)
		}

		// The SOA leads: it is what a transfer of this zone would begin with,
		// and it exists nowhere in the records table to be read from.
		if len(c.Events) != 3 {
			t.Fatalf("the commit carries %d events, want three", len(c.Events))
		}
		if c.Events[0].Type != zone.TypeSOA || !c.Events[0].Name.Equal(z.Name) {
			t.Errorf("the first event is %v, want the SOA at the apex", c.Events[0])
		}

		stored, err := f.s.ZoneByName(t.Context(), z.Name)
		if err != nil {
			t.Fatalf("ZoneByName: %v", err)
		}
		if stored.SOA.Serial != zone.NewSerial(2026081601) {
			t.Errorf("the stored zone is at serial %s", stored.SOA.Serial)
		}
		// Identifiers are minted into the slice the caller handed over, before
		// the transaction starts.
		for i := range records {
			if !id.Valid(string(records[i].ID)) {
				t.Errorf("record %d was stored with the identifier %q", i, records[i].ID)
			}
			if _, err := f.s.RecordByID(t.Context(), records[i].ID); err != nil {
				t.Errorf("record %d is not addressable by its identifier: %v", i, err)
			}
		}
	})

	t.Run("a zone with no authoritative server is refused", func(t *testing.T) {
		z := newZone(t, "nons.example.")

		_, err := f.a.CreateZone(t.Context(), z, nil, testMeta())
		if !errors.Is(err, zone.ErrInvalid) {
			t.Fatalf("CreateZone = %v, want an error wrapping zone.ErrInvalid", err)
		}
		// Nothing half-created is left behind.
		if _, err := f.s.ZoneByName(t.Context(), z.Name); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("the refused zone was stored anyway: %v", err)
		}
	})

	t.Run("a second zone with the same apex is a conflict", func(t *testing.T) {
		z := newZone(t, "example.com.")

		_, err := f.a.CreateZone(t.Context(), z, []zone.Record{apexNS(t, z, "ns1.example.com.")}, testMeta())
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("CreateZone = %v, want an error wrapping store.ErrConflict", err)
		}
	})

	t.Run("a zone starting at serial zero is refused", func(t *testing.T) {
		z := newZone(t, "zero.example.")
		z.SOA.Serial = zone.NewSerial(0)

		_, err := f.a.CreateZone(t.Context(), z, []zone.Record{apexNS(t, z, "ns1.zero.example.")}, testMeta())
		if !errors.Is(err, zone.ErrInvalid) {
			t.Fatalf("CreateZone = %v, want an error wrapping zone.ErrInvalid", err)
		}
	})

	t.Run("no source is refused", func(t *testing.T) {
		z := newZone(t, "anon.example.")

		_, err := f.a.CreateZone(t.Context(), z, []zone.Record{apexNS(t, z, "ns1.anon.example.")}, apply.Meta{})
		if !errors.Is(err, zone.ErrInvalid) {
			t.Fatalf("CreateZone = %v, want an error wrapping zone.ErrInvalid", err)
		}
	})
}

func TestUpdateZone(t *testing.T) {
	t.Parallel()

	t.Run("a change to the start of authority is a record change", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)

		before := f.serial()
		next := *f.z
		next.SOA.Expire = 1814400
		next.Comment = "extended the expiry"

		res, err := f.a.UpdateZone(t.Context(), &next, testMeta())
		if err != nil {
			t.Fatalf("UpdateZone: %v", err)
		}
		c := res.Commit()
		if c.Kind != journal.KindZoneUpdate || c.SerialTo != before.Next() {
			t.Errorf("commit = %s at %s, want a zone_update at %s", c.Kind, c.SerialTo, before.Next())
		}

		// The SOA that was served and the SOA that is now served, in the order
		// a difference sequence needs them.
		want := []string{"-", "+"}
		got := make([]string, len(c.Events))
		for i, e := range c.Events {
			if e.Type != zone.TypeSOA {
				t.Errorf("event %d is a %s, want an SOA", i, e.Type)
			}
			got[i] = e.String()[:1]
		}
		if !slices.Equal(got, want) {
			t.Errorf("events %v, want a deletion then an addition", got)
		}

		stored, err := f.s.ZoneByID(t.Context(), f.z.ID)
		if err != nil {
			t.Fatalf("ZoneByID: %v", err)
		}
		if stored.SOA.Expire != 1814400 || stored.Comment != "extended the expiry" {
			t.Errorf("the update did not take: %+v", stored)
		}
	})

	t.Run("a change to nothing served still moves the serial but carries no events", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)

		before := f.serial()
		next := *f.z
		next.Comment = "just a note"

		res, err := f.a.UpdateZone(t.Context(), &next, testMeta())
		if err != nil {
			t.Fatalf("UpdateZone: %v", err)
		}
		c := res.Commit()
		if c == nil {
			t.Fatal("a comment change produced no commit, so the audit log would not have it")
		}
		if len(c.Events) != 0 {
			t.Errorf("a comment change produced %d record events", len(c.Events))
		}
		if c.SerialTo != before.Next() {
			t.Errorf("SerialTo = %s, want %s", c.SerialTo, before.Next())
		}
	})

	t.Run("a change to nothing at all produces no commit", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)

		before := f.serial()
		same := *f.z

		res, err := f.a.UpdateZone(t.Context(), &same, testMeta())
		if err != nil {
			t.Fatalf("UpdateZone: %v", err)
		}
		if c := res.Commit(); c != nil {
			t.Errorf("an unchanged zone produced the commit %s", c.ID)
		}
		if got := f.serial(); got != before {
			t.Errorf("the serial moved from %s to %s for no change", before, got)
		}
	})

	t.Run("the caller cannot set the serial", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)

		before := f.serial()
		next := *f.z
		next.SOA.Serial = zone.NewSerial(999999)
		next.Comment = "and a serial of my choosing"

		res, err := f.a.UpdateZone(t.Context(), &next, testMeta())
		if err != nil {
			t.Fatalf("UpdateZone: %v", err)
		}
		c := res.Commit()
		// The serial belongs to the journal. A hand-set one would break the
		// one-commit-one-step rule that rollback rests on.
		if c.SerialTo != before.Next() {
			t.Errorf("SerialTo = %s, want %s", c.SerialTo, before.Next())
		}
	})

	t.Run("a rename is refused", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)

		next := *f.z
		next.Name = zone.MustParseName("elsewhere.example.")

		_, err := f.a.UpdateZone(t.Context(), &next, testMeta())
		if !errors.Is(err, zone.ErrInvalid) {
			t.Fatalf("UpdateZone = %v, want an error wrapping zone.ErrInvalid", err)
		}
	})

	t.Run("updating a zone that is not there is not found", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)

		next := newZone(t, "missing.example.")
		next.ID = zone.ZoneID(id.New())

		_, err := f.a.UpdateZone(t.Context(), next, testMeta())
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("UpdateZone = %v, want an error wrapping store.ErrNotFound", err)
		}
	})
}

func TestDeleteZone(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	f.mustApply(f.command(apply.RecordOp{
		Action: apply.ActionAdd, Record: f.record("www.example.com.", zone.TypeA, 3600, "192.0.2.10"),
	}))

	res, err := f.a.DeleteZone(t.Context(), f.z.ID,
		apply.Meta{Source: journal.SourceCLI, Actor: "tim", Comment: "decommissioned"})
	if err != nil {
		t.Fatalf("DeleteZone: %v", err)
	}
	c := res.Commit()

	if c.Kind != journal.KindZoneDelete {
		t.Errorf("Kind = %q, want %q", c.Kind, journal.KindZoneDelete)
	}
	if len(c.Events) != 0 {
		t.Errorf("the deletion carries %d events; the kind already says what happened", len(c.Events))
	}
	if verr := c.Validate(); verr != nil {
		t.Errorf("the commit does not validate: %v", verr)
	}

	if _, zerr := f.s.ZoneByID(t.Context(), f.z.ID); !errors.Is(zerr, store.ErrNotFound) {
		t.Errorf("the zone survived its deletion: %v", zerr)
	}

	// The whole point of the decision: the record of the deletion outlives what
	// it deleted, so "who removed example.com, and when" has an answer.
	got, err := f.s.CommitByID(t.Context(), c.ID)
	if err != nil {
		t.Fatalf("the record of the deletion went with the zone: %v", err)
	}
	if !got.ZoneName.Equal(f.z.Name) || got.Actor != "tim" || got.Comment != "decommissioned" {
		t.Errorf("the commit reads back as %+v", got)
	}

	// And so does everything that happened before it.
	page, err := f.s.ListCommits(t.Context(), store.CommitFilter{ZoneID: f.z.ID})
	if err != nil {
		t.Fatalf("ListCommits: %v", err)
	}
	if len(page.Items) != 3 { // create, edit, delete
		t.Errorf("the history of the deleted zone holds %d commits, want three", len(page.Items))
	}

	t.Run("deleting it twice is not found", func(t *testing.T) {
		_, err := f.a.DeleteZone(t.Context(), f.z.ID, testMeta())
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("DeleteZone = %v, want an error wrapping store.ErrNotFound", err)
		}
	})

	// A zone can be created again under the same name, and starts its own
	// history: the identifier is new, so nothing joins the two together.
	t.Run("the name can be used again", func(t *testing.T) {
		again := newZone(t, "example.com.")
		if _, err := f.a.CreateZone(t.Context(), again,
			[]zone.Record{apexNS(t, again, "ns1.example.com.")}, testMeta()); err != nil {
			t.Fatalf("CreateZone: %v", err)
		}
		page, err := f.s.ListCommits(t.Context(), store.CommitFilter{ZoneID: again.ID})
		if err != nil {
			t.Fatalf("ListCommits: %v", err)
		}
		if len(page.Items) != 1 {
			t.Errorf("the new zone starts with %d commits, want one", len(page.Items))
		}
	})
}
