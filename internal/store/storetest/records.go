package storetest

import (
	"net/netip"
	"reflect"
	"slices"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

func testRecords(t *testing.T, open Open) {
	t.Run("a stored record reads back unchanged", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		z := createZone(t, s, "example.com.")

		rec := newRecord(t, z.ID, "www.example.com.", zone.TypeA, "192.0.2.10")
		rec.Comment = "the web server"
		mustUpdate(t, s, func(tx store.Tx) error { return tx.InsertRecord(ctxOf(t), rec) })

		got, err := s.RecordByID(ctxOf(t), rec.ID)
		if err != nil {
			t.Fatalf("RecordByID: %v", err)
		}
		if !reflect.DeepEqual(rec, got) {
			t.Errorf("read back\n  %+v\nafter storing\n  %+v", got, rec)
		}
	})

	t.Run("a missing record is not found", func(t *testing.T) {
		t.Parallel()
		s := open(t)

		_, err := s.RecordByID(ctxOf(t), newID[zone.RecordID]())
		wantErrIs(t, err, store.ErrNotFound, "RecordByID on an unknown identifier")
	})

	// RFC 2181 section 5: an RRset is a set, so the same record twice is one
	// record written twice, not two.
	t.Run("the same record twice in one RRset is a conflict", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		z := createZone(t, s, "example.com.")

		createRecord(t, s, z.ID, "www.example.com.", zone.TypeA, "192.0.2.10")
		again := newRecord(t, z.ID, "www.example.com.", zone.TypeA, "192.0.2.10")

		err := updateErr(t, s, func(tx store.Tx) error { return tx.InsertRecord(ctxOf(t), again) })
		wantErrIs(t, err, store.ErrConflict, "inserting a duplicate resource record")
	})

	t.Run("the same data under a different name, type or zone is allowed", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		one := createZone(t, s, "example.com.")
		two := createZone(t, s, "example.net.")

		createRecord(t, s, one.ID, "www.example.com.", zone.TypeA, "192.0.2.10")
		createRecord(t, s, one.ID, "other.example.com.", zone.TypeA, "192.0.2.10")
		createRecord(t, s, two.ID, "www.example.net.", zone.TypeA, "192.0.2.10")
	})

	t.Run("a record without an identifier is refused", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		z := createZone(t, s, "example.com.")

		rec := newRecord(t, z.ID, "www.example.com.", zone.TypeA, "192.0.2.10")
		rec.ID = ""
		err := updateErr(t, s, func(tx store.Tx) error { return tx.InsertRecord(ctxOf(t), rec) })
		wantErrIs(t, err, zone.ErrInvalid, "inserting a record with no identifier")
	})

	t.Run("an update keeps the identity and moves only the update stamp", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		z := createZone(t, s, "example.com.")

		rec := createRecord(t, s, z.ID, "www.example.com.", zone.TypeA, "192.0.2.10")
		created := rec.CreatedAt

		changed, err := zone.NewRecord(z.ID, rec.Name, zone.ClassIN, zone.TypeA, 60, "192.0.2.11")
		if err != nil {
			t.Fatalf("building the replacement: %v", err)
		}
		changed.ID = rec.ID
		changed.Comment = "moved"
		mustUpdate(t, s, func(tx store.Tx) error { return tx.UpdateRecord(ctxOf(t), &changed) })

		got, err := s.RecordByID(ctxOf(t), rec.ID)
		if err != nil {
			t.Fatalf("RecordByID: %v", err)
		}
		if got.RData.String() != "192.0.2.11" || got.TTL != 60 || got.Comment != "moved" {
			t.Errorf("the update did not take: %+v", got)
		}
		if !got.CreatedAt.Equal(created) {
			t.Errorf("CreatedAt moved from %v to %v", created, got.CreatedAt)
		}
	})

	t.Run("updating a record that is not there is not found", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		z := createZone(t, s, "example.com.")

		rec := newRecord(t, z.ID, "www.example.com.", zone.TypeA, "192.0.2.10")
		err := updateErr(t, s, func(tx store.Tx) error { return tx.UpdateRecord(ctxOf(t), rec) })
		wantErrIs(t, err, store.ErrNotFound, "updating an unknown record")
	})

	t.Run("a record can be deleted", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		z := createZone(t, s, "example.com.")

		rec := createRecord(t, s, z.ID, "www.example.com.", zone.TypeA, "192.0.2.10")
		mustUpdate(t, s, func(tx store.Tx) error { return tx.DeleteRecord(ctxOf(t), rec.ID) })

		_, err := s.RecordByID(ctxOf(t), rec.ID)
		wantErrIs(t, err, store.ErrNotFound, "reading a deleted record")

		err = updateErr(t, s, func(tx store.Tx) error { return tx.DeleteRecord(ctxOf(t), rec.ID) })
		wantErrIs(t, err, store.ErrNotFound, "deleting the same record twice")
	})

	t.Run("an RRset is deleted as a unit", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		z := createZone(t, s, "example.com.")

		createRecord(t, s, z.ID, "www.example.com.", zone.TypeA, "192.0.2.10")
		createRecord(t, s, z.ID, "www.example.com.", zone.TypeA, "192.0.2.11")
		kept := createRecord(t, s, z.ID, "www.example.com.", zone.TypeAAAA, "2001:db8::1")

		key := zone.RRsetKey{
			Name:  zone.MustParseName("www.example.com."),
			Class: zone.ClassIN,
			Type:  zone.TypeA,
		}
		mustUpdate(t, s, func(tx store.Tx) error { return tx.DeleteRRset(ctxOf(t), z.ID, key) })

		left := drainRecords(t, s, store.RecordFilter{ZoneID: z.ID})
		if got := recordKeys(left); !slices.Equal(got, []string{"www.example.com. AAAA 2001:db8::1"}) {
			t.Errorf("after deleting the A RRset the zone holds %v", got)
		}
		if _, err := s.RecordByID(ctxOf(t), kept.ID); err != nil {
			t.Errorf("the AAAA record went too: %v", err)
		}

		// Asking for an RRset to be gone when it already is has got what it
		// asked for, unlike deleting a record by identity.
		if err := updateErr(t, s, func(tx store.Tx) error {
			return tx.DeleteRRset(ctxOf(t), z.ID, key)
		}); err != nil {
			t.Errorf("deleting an RRset that is already gone: %v", err)
		}
	})

	t.Run("addresses are indexed across zones", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		one := createZone(t, s, "example.com.")
		two := createZone(t, s, "example.net.")

		createRecord(t, s, one.ID, "www.example.com.", zone.TypeA, "192.0.2.10")
		createRecord(t, s, two.ID, "www.example.net.", zone.TypeA, "192.0.2.10")
		createRecord(t, s, one.ID, "other.example.com.", zone.TypeA, "192.0.2.11")
		createRecord(t, s, one.ID, "v6.example.com.", zone.TypeAAAA, "2001:db8::1")
		// Not an address record, and must not be found by one.
		createRecord(t, s, one.ID, "example.com.", zone.TypeNS, "ns1.example.com.")

		got, err := s.RecordsByAddress(ctxOf(t), netip.MustParseAddr("192.0.2.10"))
		if err != nil {
			t.Fatalf("RecordsByAddress: %v", err)
		}
		if want := []string{
			"www.example.com. A 192.0.2.10",
			"www.example.net. A 192.0.2.10",
		}; !slices.Equal(recordKeys(got), want) {
			t.Errorf("RecordsByAddress\n  got  %v\n  want %v", recordKeys(got), want)
		}

		v6, err := s.RecordsByAddress(ctxOf(t), netip.MustParseAddr("2001:db8::1"))
		if err != nil {
			t.Fatalf("RecordsByAddress: %v", err)
		}
		if len(v6) != 1 {
			t.Errorf("RecordsByAddress for an IPv6 address = %v, want one", recordKeys(v6))
		}

		none, err := s.RecordsByAddress(ctxOf(t), netip.MustParseAddr("10.0.0.1"))
		if err != nil {
			t.Fatalf("RecordsByAddress: %v", err)
		}
		if len(none) != 0 {
			t.Errorf("RecordsByAddress for an unused address = %v, want nothing", recordKeys(none))
		}
	})

	t.Run("a zone streams in canonical order", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		z := createZone(t, s, "example.com.")

		for _, r := range []struct {
			name  string
			typ   zone.RRType
			rdata string
		}{
			{"zeta.example.com.", zone.TypeA, "192.0.2.3"},
			{"example.com.", zone.TypeNS, "ns1.example.com."},
			{"alpha.example.com.", zone.TypeA, "192.0.2.1"},
			{"alpha.example.com.", zone.TypeAAAA, "2001:db8::1"},
			{"deep.alpha.example.com.", zone.TypeA, "192.0.2.2"},
		} {
			createRecord(t, s, z.ID, r.name, r.typ, r.rdata)
		}

		var got []string
		for rec, err := range s.IterZoneRecords(ctxOf(t), z.ID) {
			if err != nil {
				t.Fatalf("IterZoneRecords: %v", err)
			}
			got = append(got, rec.Name.String()+" "+rec.Type.String())
		}
		want := []string{
			"example.com. NS",
			"alpha.example.com. A",
			"alpha.example.com. AAAA",
			"deep.alpha.example.com. A",
			"zeta.example.com. A",
		}
		if !slices.Equal(got, want) {
			t.Errorf("stream order\n  got  %v\n  want %v", got, want)
		}
	})

	t.Run("a stream can be abandoned partway", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		z := createZone(t, s, "example.com.")
		for _, n := range []string{"a", "b", "c", "d"} {
			createRecord(t, s, z.ID, n+".example.com.", zone.TypeA, "192.0.2.1")
		}

		seen := 0
		for _, err := range s.IterZoneRecords(ctxOf(t), z.ID) {
			if err != nil {
				t.Fatalf("IterZoneRecords: %v", err)
			}
			seen++
			if seen == 2 {
				break
			}
		}
		if seen != 2 {
			t.Fatalf("the loop ran %d times, want two", seen)
		}

		// Abandoning a stream must release whatever it was holding; on a pool
		// of limited size, a leaked result set would make the next read hang
		// rather than fail.
		if _, err := s.ZoneByID(ctxOf(t), z.ID); err != nil {
			t.Errorf("reading after abandoning a stream: %v", err)
		}
	})

	t.Run("streaming an unknown zone yields nothing", func(t *testing.T) {
		t.Parallel()
		s := open(t)

		for range s.IterZoneRecords(ctxOf(t), newID[zone.ZoneID]()) {
			t.Fatal("an unknown zone produced a record")
		}
	})
}

func testManagedRecords(t *testing.T, open Open) {
	s := open(t)

	forward := createZone(t, s, "example.com.")
	reverse := createZone(t, s, "2.0.192.in-addr.arpa.")

	source := createRecord(t, s, forward.ID, "www.example.com.", zone.TypeA, "192.0.2.10")

	generated := newRecord(t, reverse.ID, "10.2.0.192.in-addr.arpa.", zone.TypePTR, "www.example.com.")
	generated.ManagedBy = source.ID
	generated.ManagedKind = zone.ManagedPTR
	mustUpdate(t, s, func(tx store.Tx) error { return tx.InsertRecord(ctxOf(t), generated) })

	t.Run("provenance reads back", func(t *testing.T) {
		got, err := s.RecordByID(ctxOf(t), generated.ID)
		if err != nil {
			t.Fatalf("RecordByID: %v", err)
		}
		if got.ManagedBy != source.ID || got.ManagedKind != zone.ManagedPTR {
			t.Errorf("provenance = %q/%q, want %q/%q",
				got.ManagedBy, got.ManagedKind, source.ID, zone.ManagedPTR)
		}
		if !got.IsManaged() {
			t.Error("the generated record does not report itself as generated")
		}
	})

	t.Run("a source names what it generated", func(t *testing.T) {
		got, err := s.ManagedBy(ctxOf(t), source.ID)
		if err != nil {
			t.Fatalf("ManagedBy: %v", err)
		}
		if len(got) != 1 || got[0].ID != generated.ID {
			t.Errorf("ManagedBy = %v, want the one generated record", recordKeys(got))
		}
	})

	t.Run("only generated records are managed", func(t *testing.T) {
		managed := drainRecords(t, s, store.RecordFilter{Managed: ref(true)})
		if got := recordKeys(managed); !slices.Equal(got,
			[]string{"10.2.0.192.in-addr.arpa. PTR www.example.com."}) {
			t.Errorf("generated records = %v", got)
		}
		authored := drainRecords(t, s, store.RecordFilter{Managed: ref(false)})
		if got := recordKeys(authored); !slices.Equal(got, []string{"www.example.com. A 192.0.2.10"}) {
			t.Errorf("authored records = %v", got)
		}
	})

	t.Run("a zone names what its records generated elsewhere", func(t *testing.T) {
		var got []string
		for rec, err := range s.ManagedByZone(ctxOf(t), forward.ID) {
			if err != nil {
				t.Fatalf("ManagedByZone: %v", err)
			}
			got = append(got, rec.Name.String())
		}
		if !slices.Equal(got, []string{"10.2.0.192.in-addr.arpa."}) {
			t.Errorf("ManagedByZone = %v, want the one generated record", got)
		}

		// Only what lives elsewhere: records inside the zone go with the zone,
		// and their removal needs no commit of its own.
		for _, err := range s.ManagedByZone(ctxOf(t), reverse.ID) {
			if err != nil {
				t.Fatalf("ManagedByZone: %v", err)
			}
			t.Error("the reverse zone reported a generated record outside itself")
		}
	})

	// The whole point of recording provenance: deleting the address record has
	// to take the reverse entry it produced with it, across zone boundaries.
	t.Run("deleting the source deletes what it generated", func(t *testing.T) {
		mustUpdate(t, s, func(tx store.Tx) error { return tx.DeleteRecord(ctxOf(t), source.ID) })

		_, err := s.RecordByID(ctxOf(t), generated.ID)
		wantErrIs(t, err, store.ErrNotFound, "reading a generated record after its source went")

		// The zone it lived in is untouched.
		if _, err := s.ZoneByID(ctxOf(t), reverse.ID); err != nil {
			t.Errorf("the reverse zone went too: %v", err)
		}
	})
}

func testRecordListing(t *testing.T, open Open) {
	s := open(t)

	z := createZone(t, s, "example.com.")
	other := createZone(t, s, "example.net.")

	createRecord(t, s, z.ID, "example.com.", zone.TypeNS, "ns1.example.com.")
	createRecord(t, s, z.ID, "www.example.com.", zone.TypeA, "192.0.2.10")
	createRecord(t, s, z.ID, "www.example.com.", zone.TypeAAAA, "2001:db8::1")
	createRecord(t, s, z.ID, "api.www.example.com.", zone.TypeA, "192.0.2.20")
	createRecord(t, s, z.ID, "mail.example.com.", zone.TypeA, "192.0.2.30")
	createRecord(t, s, z.ID, "example.com.", zone.TypeTXT, `"hello world"`)
	createRecord(t, s, other.ID, "www.example.net.", zone.TypeA, "192.0.2.10")

	t.Run("by zone", func(t *testing.T) {
		got := recordKeys(drainRecords(t, s, store.RecordFilter{ZoneID: other.ID}))
		if !slices.Equal(got, []string{"www.example.net. A 192.0.2.10"}) {
			t.Errorf("records of example.net = %v", got)
		}
	})

	t.Run("by exact name", func(t *testing.T) {
		got := recordKeys(drainRecords(t, s, store.RecordFilter{
			ZoneID: z.ID,
			Name:   zone.MustParseName("www.example.com."),
		}))
		want := []string{"www.example.com. A 192.0.2.10", "www.example.com. AAAA 2001:db8::1"}
		if !slices.Equal(got, want) {
			t.Errorf("records at www\n  got  %v\n  want %v", got, want)
		}
	})

	// One indexed range over the sort key, which is what makes expanding a
	// branch of the name tree cheap.
	t.Run("by branch", func(t *testing.T) {
		got := recordKeys(drainRecords(t, s, store.RecordFilter{
			ZoneID: z.ID,
			Under:  zone.MustParseName("www.example.com."),
		}))
		want := []string{
			"www.example.com. A 192.0.2.10",
			"www.example.com. AAAA 2001:db8::1",
			"api.www.example.com. A 192.0.2.20",
		}
		if !slices.Equal(got, want) {
			t.Errorf("records under www\n  got  %v\n  want %v", got, want)
		}
	})

	t.Run("by type", func(t *testing.T) {
		got := recordKeys(drainRecords(t, s, store.RecordFilter{
			ZoneID: z.ID,
			Types:  []zone.RRType{zone.TypeAAAA, zone.TypeNS},
		}))
		want := []string{"example.com. NS ns1.example.com.", "www.example.com. AAAA 2001:db8::1"}
		if !slices.Equal(got, want) {
			t.Errorf("NS and AAAA\n  got  %v\n  want %v", got, want)
		}
	})

	// This is how a reverse zone finds the records it should be answering for.
	// Blob ordering compares bytes before length, so a sixteen-byte address can
	// otherwise fall inside a four-byte range: 2001:db8:: begins with the same
	// two bytes as 32.1.13.184.
	t.Run("by network", func(t *testing.T) {
		got := recordKeys(drainRecords(t, s, store.RecordFilter{
			Prefix: netip.MustParsePrefix("192.0.2.0/28"),
		}))
		want := []string{
			"www.example.com. A 192.0.2.10",
			"www.example.net. A 192.0.2.10",
		}
		if !slices.Equal(got, want) {
			t.Errorf("records in 192.0.2.0/28\n  got  %v\n  want %v", got, want)
		}

		wider := recordKeys(drainRecords(t, s, store.RecordFilter{
			Prefix: netip.MustParsePrefix("192.0.2.0/24"),
		}))
		if len(wider) != 4 {
			t.Errorf("records in 192.0.2.0/24 = %v, want four", wider)
		}

		if got := drainRecords(t, s, store.RecordFilter{
			Prefix: netip.MustParsePrefix("10.0.0.0/8"),
		}); len(got) != 0 {
			t.Errorf("records in 10.0.0.0/8 = %v, want none", recordKeys(got))
		}

		// The IPv6 record is in none of the IPv4 ranges above, and is found by
		// its own network.
		six := recordKeys(drainRecords(t, s, store.RecordFilter{
			Prefix: netip.MustParsePrefix("2001:db8::/32"),
		}))
		if !slices.Equal(six, []string{"www.example.com. AAAA 2001:db8::1"}) {
			t.Errorf("records in 2001:db8::/32 = %v", six)
		}
	})

	t.Run("by search over names and data alike", func(t *testing.T) {
		byName := recordKeys(drainRecords(t, s, store.RecordFilter{ZoneID: z.ID, Search: "MAIL"}))
		if !slices.Equal(byName, []string{"mail.example.com. A 192.0.2.30"}) {
			t.Errorf("search for MAIL = %v", byName)
		}
		byData := recordKeys(drainRecords(t, s, store.RecordFilter{ZoneID: z.ID, Search: "hello"}))
		if !slices.Equal(byData, []string{`example.com. TXT "hello world"`}) {
			t.Errorf("search for hello = %v", byData)
		}
	})

	t.Run("one record per page reaches the same list", func(t *testing.T) {
		full := recordKeys(drainRecords(t, s, store.RecordFilter{ZoneID: z.ID}))
		paged := recordKeys(drainRecords(t, s, store.RecordFilter{
			ZoneID: z.ID,
			Paging: store.Paging{Limit: 1},
		}))
		if !slices.Equal(full, paged) {
			t.Errorf("paging changed the listing\n  one page     %v\n  page by page %v", full, paged)
		}
		if len(full) != 6 {
			t.Errorf("the zone holds %d records, want six", len(full))
		}
	})

	t.Run("one name, ordered by type and paged through", func(t *testing.T) {
		// Its own case, and its own records, because a listing narrowed to one
		// owner name is its own query: every row shares a sort key there, so a
		// backend that orders and pages by sort key has nothing to work with
		// and must fall back on something else. Getting that wrong reorders the
		// RRsets at a name, or loses and repeats records across pages, and the
		// whole-zone case above sees neither.
		//
		// The records go in against the order they must come out in, TXT is
		// type 16 and A is type 1, so a backend that returns them in the order
		// it stored them, or by identifier, fails here instead of passing by
		// coincidence.
		pz := createZone(t, s, "paged.example.")
		name := "one.paged.example."
		createRecord(t, s, pz.ID, name, zone.TypeTXT, `"third by type"`)
		createRecord(t, s, pz.ID, name, zone.TypeA, "192.0.2.1")
		createRecord(t, s, pz.ID, name, zone.TypeAAAA, "2001:db8::1")

		want := []string{
			name + " A 192.0.2.1",
			name + ` TXT "third by type"`,
			name + " AAAA 2001:db8::1",
		}

		f := store.RecordFilter{ZoneID: pz.ID, Name: zone.MustParseName(name)}
		full := recordKeys(drainRecords(t, s, f))
		if !slices.Equal(full, want) {
			t.Errorf("records at one name\n  got  %v\n  want %v", full, want)
		}

		f.Paging = store.Paging{Limit: 1}
		paged := recordKeys(drainRecords(t, s, f))
		if !slices.Equal(paged, want) {
			t.Errorf("paging changed the listing at one name\n  one at a time %v\n  want          %v",
				paged, want)
		}
	})

	t.Run("a page never exceeds the maximum", func(t *testing.T) {
		page, err := s.ListRecords(ctxOf(t), store.RecordFilter{
			ZoneID: z.ID,
			Paging: store.Paging{Limit: store.MaxLimit + 1000},
		})
		if err != nil {
			t.Fatalf("ListRecords: %v", err)
		}
		if len(page.Items) > store.MaxLimit {
			t.Errorf("a page of %d exceeds the maximum of %d", len(page.Items), store.MaxLimit)
		}
	})
}
