package apply_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/apply"
	"github.com/wegweiserzone/wegweiser/internal/journal"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// importMeta is how a change that came out of a file records itself.
func importMeta() apply.Meta {
	return apply.Meta{Source: journal.SourceImport, Actor: "deploy-token", Comment: "import example.com"}
}

func importOf(t *testing.T, apex string, lines ...string) apply.Import {
	t.Helper()

	name := zone.MustParseName(apex)
	in := apply.Import{
		Name: name,
		SOA: zone.SOA{
			NS: zone.MustParseName("ns1." + apex), Mbox: zone.MustParseName("hostmaster." + apex),
			Serial: zone.NewSerial(2026081801), Refresh: 7200, Retry: 900,
			Expire: 1209600, Minimum: 3600, TTL: 3600,
		},
	}
	for _, l := range lines {
		f := strings.Fields(l)
		typ, err := zone.ParseRRType(f[1])
		if err != nil {
			t.Fatalf("type in %q: %v", l, err)
		}
		r, err := zone.NewRecord("", zone.MustParseName(f[0]), zone.ClassIN, typ, 3600,
			strings.Join(f[2:], " "))
		if err != nil {
			t.Fatalf("record %q: %v", l, err)
		}
		in.Records = append(in.Records, r)
	}
	return in
}

// TestImportSeedsTheSerial is the test docs/decisions.md D2 asks for by name.
// Starting a migrated zone at 1 is the single most common way to break a
// migration: every secondary that has seen the old serial considers our copy
// older (RFC 1982 §3.2) and refuses to transfer it.
func TestImportSeedsTheSerial(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	in := importOf(t, "migrated.example.",
		"migrated.example. NS ns1.migrated.example.",
		"www.migrated.example. A 192.0.2.10",
	)

	res, err := f.a.Import(f.t.Context(), in, importMeta())
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	z, err := f.s.ZoneByName(f.t.Context(), in.Name)
	if err != nil {
		t.Fatalf("ZoneByName: %v", err)
	}
	if z.SOA.Serial != in.SOA.Serial {
		t.Errorf("the zone starts at serial %s, want the %s it arrived with",
			z.SOA.Serial, in.SOA.Serial)
	}

	t.Run("the first commit says where it started", func(t *testing.T) {
		c := res.Commit()
		if c.SerialTo != in.SOA.Serial {
			t.Errorf("the first commit ends at %s, want %s", c.SerialTo, in.SOA.Serial)
		}
		if c.Source != journal.SourceImport {
			t.Errorf("source = %q, want the interface it arrived through", c.Source)
		}
	})

	t.Run("the next change carries on from there", func(t *testing.T) {
		rec, rerr := zone.NewRecord(z.ID, zone.MustParseName("later.migrated.example."),
			zone.ClassIN, zone.TypeA, 300, "192.0.2.11")
		if rerr != nil {
			t.Fatalf("NewRecord: %v", rerr)
		}
		if _, aerr := f.a.Apply(f.t.Context(), apply.Command{
			ZoneID: z.ID, Ops: []apply.RecordOp{{Action: apply.ActionAdd, Record: &rec}},
			Kind: journal.KindEdit, Source: journal.SourceAPI, Actor: "someone",
		}); aerr != nil {
			t.Fatalf("Apply: %v", aerr)
		}

		after, zerr := f.s.ZoneByName(f.t.Context(), in.Name)
		if zerr != nil {
			t.Fatalf("ZoneByName: %v", zerr)
		}
		if after.SOA.Serial != in.SOA.Serial.Next() {
			t.Errorf("serial = %s, want one step on from %s", after.SOA.Serial, in.SOA.Serial)
		}
	})
}

func TestImportSkipsWhatCouldNeverBeAnswered(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	// A real zone: a delegation, its glue, and, as real zones have, a record
	// below the delegation that the parent will never answer with.
	in := importOf(t, "occluded.example.",
		"occluded.example. NS ns1.occluded.example.",
		"ns1.occluded.example. A 192.0.2.1",
		"sub.occluded.example. NS ns1.sub.occluded.example.",
		"ns1.sub.occluded.example. A 192.0.2.53",
		"www.sub.occluded.example. TXT \"never answered\"",
		"sub.occluded.example. TXT \"nor this\"",
	)

	res, err := f.a.Import(f.t.Context(), in, importMeta())
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	t.Run("the file is not refused over it", func(t *testing.T) {
		// A migration that fails on the whole file because of one such record
		// is a migration nobody completes.
		if !res.Changed() {
			t.Fatal("nothing was imported")
		}
	})

	t.Run("what was left out comes back as data", func(t *testing.T) {
		if len(res.Skipped) != 2 {
			t.Fatalf("skipped %d records, want the TXT at and below the delegation: %+v",
				len(res.Skipped), res.Skipped)
		}
		for _, s := range res.Skipped {
			if s.Record.Type != zone.TypeTXT {
				t.Errorf("skipped %s, want only the TXT records", s.Record)
			}
			if !strings.Contains(s.Reason, "sub.occluded.example.") {
				t.Errorf("reason = %q, want it to name the delegation", s.Reason)
			}
		}
	})

	t.Run("the glue below the delegation stays", func(t *testing.T) {
		// Without it the child's servers cannot be reached at all.
		page, perr := f.s.ListRecords(f.t.Context(), store.RecordFilter{
			Name: zone.MustParseName("ns1.sub.occluded.example."),
		})
		if perr != nil {
			t.Fatalf("ListRecords: %v", perr)
		}
		if len(page.Items) != 1 {
			t.Errorf("the glue is not there: %+v", page.Items)
		}
	})

	t.Run("the delegation itself stays", func(t *testing.T) {
		page, perr := f.s.ListRecords(f.t.Context(), store.RecordFilter{
			Name:  zone.MustParseName("sub.occluded.example."),
			Types: []zone.RRType{zone.TypeNS},
		})
		if perr != nil {
			t.Fatalf("ListRecords: %v", perr)
		}
		if len(page.Items) != 1 {
			t.Errorf("the delegation went with the records under it: %+v", page.Items)
		}
	})
}

func TestImportRefusals(t *testing.T) {
	t.Parallel()

	t.Run("into a zone that already exists", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)

		// A file is the complete contents of a zone, so this is a replacement
		// rather than an import, and doing it silently is the difference
		// between gaining a zone and losing one.
		in := importOf(t, "example.com.", "example.com. NS ns1.example.com.")
		_, err := f.a.Import(f.t.Context(), in, importMeta())
		if !errors.Is(err, store.ErrConflict) {
			t.Errorf("error = %v, want a conflict", err)
		}
	})

	t.Run("with no NS at the apex", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)

		in := importOf(t, "nameless.example.", "www.nameless.example. A 192.0.2.1")
		if _, err := f.a.Import(f.t.Context(), in, importMeta()); err == nil {
			t.Error("imported a zone that names no authority for itself (RFC 1034 §4.2.1)")
		}
	})

	t.Run("with a start of authority that is not usable", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)

		in := importOf(t, "broken.example.", "broken.example. NS ns1.broken.example.")
		in.SOA.Expire = 0
		if _, err := f.a.Import(f.t.Context(), in, importMeta()); err == nil {
			t.Error("imported a zone whose secondaries would discard it")
		}
	})
}

func TestImportGeneratesReverse(t *testing.T) {
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

	in := importOf(t, "reverse.example.",
		"reverse.example. NS ns1.reverse.example.",
		"www.reverse.example. A 192.0.2.10",
	)
	if _, ierr := f.a.Import(f.t.Context(), in, importMeta()); ierr != nil {
		t.Fatalf("Import: %v", ierr)
	}

	// An imported zone is a zone, so the automation that runs for every other
	// address record runs for these too.
	page, err := f.s.ListRecords(f.t.Context(), store.RecordFilter{
		ZoneID: rev.ID, Types: []zone.RRType{zone.TypePTR},
	})
	if err != nil {
		t.Fatalf("ListRecords: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].RData.String() != "www.reverse.example." {
		t.Errorf("the reverse zone holds %+v, want the PTR the import caused", page.Items)
	}
}
