package storetest

import (
	"net/netip"
	"reflect"
	"slices"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

func testZones(t *testing.T, open Open) {
	t.Run("a stored zone reads back unchanged", func(t *testing.T) {
		t.Parallel()
		s := open(t)

		z := newZone(t, "example.com.")
		z.Comment = "the first one"
		z.DefaultTTL = 900
		z.AutoReverse = ref(true)
		mustUpdate(t, s, func(tx store.Tx) error { return tx.CreateZone(ctxOf(t), z) })

		if z.CreatedAt.IsZero() || z.UpdatedAt.IsZero() {
			t.Fatal("the store did not stamp the zone it was handed")
		}

		got, err := s.ZoneByID(ctxOf(t), z.ID)
		if err != nil {
			t.Fatalf("ZoneByID: %v", err)
		}
		// The value handed in and the value read back have to be the same
		// thing, timestamps included; anything else means the store kept
		// something the caller cannot see.
		if !reflect.DeepEqual(z, got) {
			t.Errorf("read back\n  %+v\nafter storing\n  %+v", got, z)
		}

		byName, err := s.ZoneByName(ctxOf(t), z.Name)
		if err != nil {
			t.Fatalf("ZoneByName: %v", err)
		}
		if byName.ID != z.ID {
			t.Errorf("ZoneByName found %s, want %s", byName.ID, z.ID)
		}
	})

	t.Run("a reverse zone keeps its network", func(t *testing.T) {
		t.Parallel()
		s := open(t)

		z := createZone(t, s, "2.0.192.in-addr.arpa.")
		got, err := s.ZoneByID(ctxOf(t), z.ID)
		if err != nil {
			t.Fatalf("ZoneByID: %v", err)
		}
		if got.Kind != zone.KindReverse {
			t.Errorf("Kind = %q, want %q", got.Kind, zone.KindReverse)
		}
		if want := netip.MustParsePrefix("192.0.2.0/24"); got.Prefix != want {
			t.Errorf("Prefix = %v, want %v", got.Prefix, want)
		}
	})

	t.Run("a missing zone is not found", func(t *testing.T) {
		t.Parallel()
		s := open(t)

		_, err := s.ZoneByID(ctxOf(t), newID[zone.ZoneID]())
		wantErrIs(t, err, store.ErrNotFound, "ZoneByID on an unknown identifier")

		_, err = s.ZoneByName(ctxOf(t), zone.MustParseName("nothing.example."))
		wantErrIs(t, err, store.ErrNotFound, "ZoneByName on an unknown name")
	})

	t.Run("a second zone with the same apex is a conflict", func(t *testing.T) {
		t.Parallel()
		s := open(t)

		createZone(t, s, "example.com.")
		other := newZone(t, "example.com.")

		err := updateErr(t, s, func(tx store.Tx) error { return tx.CreateZone(ctxOf(t), other) })
		wantErrIs(t, err, store.ErrConflict, "creating a duplicate apex")
	})

	t.Run("a zone without an identifier is refused", func(t *testing.T) {
		t.Parallel()
		s := open(t)

		z := newZone(t, "example.com.")
		z.ID = ""
		err := updateErr(t, s, func(tx store.Tx) error { return tx.CreateZone(ctxOf(t), z) })
		wantErrIs(t, err, zone.ErrInvalid, "creating a zone with no identifier")
	})

	t.Run("an invalid zone is refused", func(t *testing.T) {
		t.Parallel()
		s := open(t)

		z := newZone(t, "example.com.")
		z.SOA.Refresh = 0 // expire can no longer exceed refresh plus retry
		err := updateErr(t, s, func(tx store.Tx) error { return tx.CreateZone(ctxOf(t), z) })
		wantErrIs(t, err, zone.ErrInvalid, "creating a zone with a broken SOA")
	})

	t.Run("an update replaces settings and moves only the update stamp", func(t *testing.T) {
		t.Parallel()
		s := open(t)

		z := createZone(t, s, "example.com.")
		created := z.CreatedAt

		z.Comment = "renamed"
		z.DefaultTTL = 60
		z.Disabled = true
		z.AutoReverse = ref(false)
		mustUpdate(t, s, func(tx store.Tx) error { return tx.UpdateZone(ctxOf(t), z) })

		got, err := s.ZoneByID(ctxOf(t), z.ID)
		if err != nil {
			t.Fatalf("ZoneByID: %v", err)
		}
		if !got.CreatedAt.Equal(created) {
			t.Errorf("CreatedAt moved from %v to %v", created, got.CreatedAt)
		}
		if got.Comment != "renamed" || got.DefaultTTL != 60 || !got.Disabled {
			t.Errorf("the update did not take: %+v", got)
		}
		if got.AutoReverse == nil || *got.AutoReverse {
			t.Errorf("AutoReverse = %v, want false", got.AutoReverse)
		}
	})

	t.Run("updating a zone that is not there is not found", func(t *testing.T) {
		t.Parallel()
		s := open(t)

		z := newZone(t, "example.com.")
		err := updateErr(t, s, func(tx store.Tx) error { return tx.UpdateZone(ctxOf(t), z) })
		wantErrIs(t, err, store.ErrNotFound, "updating an unknown zone")
	})

	t.Run("the serial advances on its own", func(t *testing.T) {
		t.Parallel()
		s := open(t)

		z := createZone(t, s, "example.com.")
		next := z.SOA.Serial.Next()
		mustUpdate(t, s, func(tx store.Tx) error { return tx.SetZoneSerial(ctxOf(t), z.ID, next) })

		got, err := s.ZoneByID(ctxOf(t), z.ID)
		if err != nil {
			t.Fatalf("ZoneByID: %v", err)
		}
		if got.SOA.Serial != next {
			t.Errorf("serial = %s, want %s", got.SOA.Serial, next)
		}

		err = updateErr(t, s, func(tx store.Tx) error {
			return tx.SetZoneSerial(ctxOf(t), newID[zone.ZoneID](), next)
		})
		wantErrIs(t, err, store.ErrNotFound, "advancing the serial of an unknown zone")
	})

	// A serial is a 32-bit value that wraps, and a database column is signed.
	// The one that wraps has to survive the round trip through the one that
	// does not.
	t.Run("a serial near the top of its range survives", func(t *testing.T) {
		t.Parallel()
		s := open(t)

		z := createZone(t, s, "example.com.")
		high := zone.NewSerial(^uint32(0))
		mustUpdate(t, s, func(tx store.Tx) error { return tx.SetZoneSerial(ctxOf(t), z.ID, high) })

		got, err := s.ZoneByID(ctxOf(t), z.ID)
		if err != nil {
			t.Fatalf("ZoneByID: %v", err)
		}
		if got.SOA.Serial != high {
			t.Errorf("serial = %s, want %s", got.SOA.Serial, high)
		}
	})

	t.Run("deleting a zone takes its records with it", func(t *testing.T) {
		t.Parallel()
		s := open(t)

		z := createZone(t, s, "example.com.")
		rec := createRecord(t, s, z.ID, "www.example.com.", zone.TypeA, "192.0.2.10")

		mustUpdate(t, s, func(tx store.Tx) error { return tx.DeleteZone(ctxOf(t), z.ID) })

		_, err := s.ZoneByID(ctxOf(t), z.ID)
		wantErrIs(t, err, store.ErrNotFound, "reading a deleted zone")

		// This is the check that fails silently when foreign keys are off, so
		// it is the reason the connection settings are verified at startup.
		_, err = s.RecordByID(ctxOf(t), rec.ID)
		wantErrIs(t, err, store.ErrNotFound, "reading a record of a deleted zone")
	})

	t.Run("deleting a zone that is not there is not found", func(t *testing.T) {
		t.Parallel()
		s := open(t)

		err := updateErr(t, s, func(tx store.Tx) error {
			return tx.DeleteZone(ctxOf(t), newID[zone.ZoneID]())
		})
		wantErrIs(t, err, store.ErrNotFound, "deleting an unknown zone")
	})
}

func testZoneListing(t *testing.T, open Open) {
	s := open(t)

	// Deliberately created out of order, so that the listing is doing the
	// ordering rather than the insert sequence.
	for _, apex := range []string{
		"zeta.example.", "alpha.example.", "sub.alpha.example.",
		"2.0.192.in-addr.arpa.", "8.b.d.0.1.0.0.2.ip6.arpa.",
	} {
		createZone(t, s, apex)
	}
	disabled := createZone(t, s, "off.example.")
	disabled.Disabled = true
	mustUpdate(t, s, func(tx store.Tx) error { return tx.UpdateZone(ctxOf(t), disabled) })

	t.Run("canonical order", func(t *testing.T) {
		got := zoneNames(drainZones(t, s, store.ZoneFilter{}))
		// RFC 4034 section 6.1 orders by reversed labels, so arpa comes before
		// example, and a name sorts before the names below it.
		want := []string{
			"2.0.192.in-addr.arpa.",
			"8.b.d.0.1.0.0.2.ip6.arpa.",
			"alpha.example.",
			"sub.alpha.example.",
			"off.example.",
			"zeta.example.",
		}
		if !slices.Equal(got, want) {
			t.Errorf("listing order\n  got  %v\n  want %v", got, want)
		}
	})

	t.Run("the stream reaches the same list without a cursor", func(t *testing.T) {
		want := zoneNames(drainZones(t, s, store.ZoneFilter{}))

		var got []string
		for z, err := range s.IterZones(ctxOf(t)) {
			if err != nil {
				t.Fatalf("IterZones: %v", err)
			}
			got = append(got, z.Name.String())
		}
		if !slices.Equal(got, want) {
			t.Errorf("stream order\n  got  %v\n  want %v", got, want)
		}
	})

	t.Run("a zone stream can be abandoned partway", func(t *testing.T) {
		seen := 0
		for _, err := range s.IterZones(ctxOf(t)) {
			if err != nil {
				t.Fatalf("IterZones: %v", err)
			}
			seen++
			if seen == 2 {
				break
			}
		}
		if seen != 2 {
			t.Fatalf("the loop ran %d times, want two", seen)
		}

		// Abandoning a stream has to release what it held. On a pool of limited
		// size a leaked result set makes the next read hang rather than fail.
		if _, err := s.ListZones(ctxOf(t), store.ZoneFilter{}); err != nil {
			t.Errorf("reading after abandoning a stream: %v", err)
		}
	})

	t.Run("one zone per page reaches the same list", func(t *testing.T) {
		full := zoneNames(drainZones(t, s, store.ZoneFilter{}))
		paged := zoneNames(drainZones(t, s, store.ZoneFilter{Paging: store.Paging{Limit: 1}}))
		if !slices.Equal(full, paged) {
			t.Errorf("paging changed the listing\n  one page  %v\n  page by page %v", full, paged)
		}
	})

	t.Run("filter by kind", func(t *testing.T) {
		got := zoneNames(drainZones(t, s, store.ZoneFilter{Kind: zone.KindReverse}))
		want := []string{"2.0.192.in-addr.arpa.", "8.b.d.0.1.0.0.2.ip6.arpa."}
		if !slices.Equal(got, want) {
			t.Errorf("reverse zones\n  got  %v\n  want %v", got, want)
		}
	})

	t.Run("filter by name, exactly", func(t *testing.T) {
		// What a client does with a name a person typed, before it can do
		// anything else with it. Search cannot serve that: "alpha.example."
		// also matches "sub.alpha.example.".
		got := zoneNames(drainZones(t, s, store.ZoneFilter{
			Name: zone.MustParseName("alpha.example."),
		}))
		if !slices.Equal(got, []string{"alpha.example."}) {
			t.Errorf("the zone named alpha.example.\n  got  %v\n  want [alpha.example.]", got)
		}

		none := drainZones(t, s, store.ZoneFilter{Name: zone.MustParseName("nowhere.example.")})
		if len(none) != 0 {
			t.Errorf("a name no zone has matched %v", zoneNames(none))
		}
	})

	t.Run("filter by search", func(t *testing.T) {
		got := zoneNames(drainZones(t, s, store.ZoneFilter{Search: "ALPHA"}))
		want := []string{"alpha.example.", "sub.alpha.example."}
		if !slices.Equal(got, want) {
			t.Errorf("search for ALPHA\n  got  %v\n  want %v", got, want)
		}
	})

	// A percent sign is a wildcard in LIKE and a plain character to a user.
	t.Run("search treats wildcards as characters", func(t *testing.T) {
		got := drainZones(t, s, store.ZoneFilter{Search: "%"})
		if len(got) != 0 {
			t.Errorf("searching for %%%% matched %v, want nothing", zoneNames(got))
		}
	})

	t.Run("filter by disabled", func(t *testing.T) {
		got := zoneNames(drainZones(t, s, store.ZoneFilter{Disabled: ref(true)}))
		if !slices.Equal(got, []string{"off.example."}) {
			t.Errorf("disabled zones = %v, want [off.example.]", got)
		}
		enabled := drainZones(t, s, store.ZoneFilter{Disabled: ref(false)})
		if len(enabled) != 5 {
			t.Errorf("enabled zones = %v, want five", zoneNames(enabled))
		}
	})
}

// testEmptyZoneStream is separate because testZoneListing works against a store
// that already holds zones, and "yields nothing" needs one that does not.
func testEmptyZoneStream(t *testing.T, open Open) {
	s := open(t)

	for range s.IterZones(ctxOf(t)) {
		t.Fatal("an empty store produced a zone")
	}
}

func testReverseZones(t *testing.T, open Open) {
	s := open(t)

	createZone(t, s, "example.com.")               // forward, must never match
	createZone(t, s, "0.192.in-addr.arpa.")        // 192.0.0.0/16
	createZone(t, s, "2.0.192.in-addr.arpa.")      // 192.0.2.0/24
	createZone(t, s, "0/25.2.0.192.in-addr.arpa.") // 192.0.2.0/25, RFC 2317
	createZone(t, s, "8.b.d.0.1.0.0.2.ip6.arpa.")  // 2001:db8::/32

	tests := []struct {
		addr string
		want string
	}{
		// The classless child is more specific than the /24 that delegates to
		// it, and a longest-prefix match has to prefer it.
		{"192.0.2.1", "0/25.2.0.192.in-addr.arpa."},
		{"192.0.2.200", "2.0.192.in-addr.arpa."},
		{"192.0.9.1", "0.192.in-addr.arpa."},
		{"2001:db8::1", "8.b.d.0.1.0.0.2.ip6.arpa."},
	}

	for _, tc := range tests {
		t.Run(tc.addr, func(t *testing.T) {
			got, err := s.ReverseZoneFor(ctxOf(t), netip.MustParseAddr(tc.addr))
			if err != nil {
				t.Fatalf("ReverseZoneFor(%s): %v", tc.addr, err)
			}
			if got.Name.String() != tc.want {
				t.Errorf("ReverseZoneFor(%s) = %s, want %s", tc.addr, got.Name, tc.want)
			}
		})
	}

	t.Run("an address no zone covers is not found", func(t *testing.T) {
		_, err := s.ReverseZoneFor(ctxOf(t), netip.MustParseAddr("10.0.0.1"))
		wantErrIs(t, err, store.ErrNotFound, "ReverseZoneFor outside every zone")
	})

	// The same address written as IPv4 and as IPv4-mapped IPv6 has to reach the
	// same zone; the mapped form is sixteen bytes and would match no IPv4
	// network unless it is recognised.
	t.Run("an IPv4-mapped address finds the IPv4 zone", func(t *testing.T) {
		got, err := s.ReverseZoneFor(ctxOf(t), netip.MustParseAddr("::ffff:192.0.2.200"))
		if err != nil {
			t.Fatalf("ReverseZoneFor: %v", err)
		}
		if got.Name.String() != "2.0.192.in-addr.arpa." {
			t.Errorf("ReverseZoneFor(::ffff:192.0.2.200) = %s, want 2.0.192.in-addr.arpa.", got.Name)
		}
	})
}
