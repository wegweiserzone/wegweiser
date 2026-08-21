// Package storetest is the conformance suite every [store.Store]
// implementation has to pass.
package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/id"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// Open returns a store that is migrated, empty, and cleaned up when the test
// ends.
type Open func(t *testing.T) store.Store

// Run exercises the whole [store.Store] contract.
func Run(t *testing.T, open Open) {
	t.Helper()

	suites := map[string]func(*testing.T, Open){
		"Zones":           testZones,
		"ZoneListing":     testZoneListing,
		"EmptyZoneStream": testEmptyZoneStream,
		"ReverseZones":    testReverseZones,
		"Records":         testRecords,
		"RecordListing":   testRecordListing,
		"ManagedRecords":  testManagedRecords,
		"Journal":         testJournal,
		"Tokens":          testTokens,
		"Settings":        testSettings,
		"Transactions":    testTransactions,
		"Cursors":         testCursors,
	}
	for name, suite := range suites {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			suite(t, open)
		})
	}
}

// --------------------------------------------------------------------- setup

// newID mints an identifier of whatever identifier type is wanted.
func newID[T ~string]() T { return T(id.New()) }

// testSOA is a valid start of authority to hang test zones off.
func testSOA() zone.SOA {
	return zone.DefaultSOA(
		zone.MustParseName("ns1.example.com."),
		zone.MustParseName("hostmaster.example.com."),
	)
}

// newZone builds an unsaved zone for the given apex.
func newZone(t *testing.T, apex string) *zone.Zone {
	t.Helper()

	z, err := zone.NewZone(zone.MustParseName(apex), testSOA())
	if err != nil {
		t.Fatalf("building the zone %s: %v", apex, err)
	}
	z.ID = newID[zone.ZoneID]()
	return &z
}

// createZone builds and stores a zone.
func createZone(t *testing.T, s store.Store, apex string) *zone.Zone {
	t.Helper()

	z := newZone(t, apex)
	mustUpdate(t, s, func(tx store.Tx) error { return tx.CreateZone(t.Context(), z) })
	return z
}

// newRecord builds an unsaved record.
func newRecord(t *testing.T, zid zone.ZoneID, name string, typ zone.RRType, rdata string) *zone.Record {
	t.Helper()

	rec, err := zone.NewRecord(zid, zone.MustParseName(name), zone.ClassIN, typ, 3600, rdata)
	if err != nil {
		t.Fatalf("building the record %s %s %s: %v", name, typ, rdata, err)
	}
	rec.ID = newID[zone.RecordID]()
	return &rec
}

// createRecord builds and stores a record.
func createRecord(t *testing.T, s store.Store, zid zone.ZoneID, name string, typ zone.RRType, rdata string) *zone.Record {
	t.Helper()

	rec := newRecord(t, zid, name, typ, rdata)
	mustUpdate(t, s, func(tx store.Tx) error { return tx.InsertRecord(t.Context(), rec) })
	return rec
}

// mustUpdate runs a write transaction that has to succeed.
func mustUpdate(t *testing.T, s store.Store, fn func(store.Tx) error) {
	t.Helper()

	if err := s.Update(t.Context(), fn); err != nil {
		t.Fatalf("write transaction: %v", err)
	}
}

// updateErr runs a write transaction and returns its error.
func updateErr(t *testing.T, s store.Store, fn func(store.Tx) error) error {
	t.Helper()
	return s.Update(t.Context(), fn)
}

// wantErrIs fails unless err matches target.
func wantErrIs(t *testing.T, err, target error, what string) {
	t.Helper()

	if !errors.Is(err, target) {
		t.Fatalf("%s: error = %v, want one wrapping %v", what, err, target)
	}
}

// zoneNames renders a page of zones for comparison in a failure message.
func zoneNames(zones []*zone.Zone) []string {
	out := make([]string, len(zones))
	for i, z := range zones {
		out[i] = z.Name.String()
	}
	return out
}

// recordKeys renders a page of records for comparison.
func recordKeys(records []*zone.Record) []string {
	out := make([]string, len(records))
	for i, r := range records {
		out[i] = r.Name.String() + " " + r.Type.String() + " " + r.RData.String()
	}
	return out
}

// drainZones collects a whole zone listing one page at a time, so that paging
// is exercised by every test that needs the full set.
func drainZones(t *testing.T, s store.Store, f store.ZoneFilter) []*zone.Zone {
	t.Helper()

	var out []*zone.Zone
	seen := map[store.Cursor]bool{}
	for {
		page, err := s.ListZones(t.Context(), f)
		if err != nil {
			t.Fatalf("ListZones: %v", err)
		}
		out = append(out, page.Items...)
		if page.NextCursor == "" {
			return out
		}
		if seen[page.NextCursor] {
			t.Fatalf("ListZones returned the cursor %q twice, so paging does not advance", page.NextCursor)
		}
		seen[page.NextCursor] = true
		f.Cursor = page.NextCursor
	}
}

// drainRecords collects a whole record listing one page at a time.
func drainRecords(t *testing.T, s store.Store, f store.RecordFilter) []*zone.Record {
	t.Helper()

	var out []*zone.Record
	seen := map[store.Cursor]bool{}
	for {
		page, err := s.ListRecords(t.Context(), f)
		if err != nil {
			t.Fatalf("ListRecords: %v", err)
		}
		out = append(out, page.Items...)
		if page.NextCursor == "" {
			return out
		}
		if seen[page.NextCursor] {
			t.Fatalf("ListRecords returned the cursor %q twice, so paging does not advance", page.NextCursor)
		}
		seen[page.NextCursor] = true
		f.Cursor = page.NextCursor
	}
}

// ctxOf keeps the context plumbing out of the individual cases.
func ctxOf(t *testing.T) context.Context { return t.Context() }

// ref returns a pointer to v, for the filters and fields that use one to mean
// "set" rather than "unset".
func ref[T any](v T) *T { return &v }

// truncMillis rounds a time to what the store keeps, so a test can compare a
// value it supplied with the value that comes back.
func truncMillis(t time.Time) time.Time { return time.UnixMilli(t.UnixMilli()).UTC() }
