package sqlite_test

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/id"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/store/sqlite"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// Reverse automation asks this question for every address record it sees, so
// what one lookup costs, and how that cost grows with the number of zones, is
// the number that decides whether it can be asked per record at all.
func BenchmarkReverseZoneFor(b *testing.B) {
	for _, zones := range []int{10, 100, 2000} {
		for _, addr := range []string{"10.0.5.7", "2001:db8::1"} {
			b.Run(fmt.Sprintf("%dzones/%s", zones, addr), func(b *testing.B) {
				s := benchStore(b, zones)
				want := netip.MustParseAddr(addr)

				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					if _, err := s.ReverseZoneFor(b.Context(), want); err != nil &&
						!errors.Is(err, store.ErrNotFound) {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// benchStore opens a migrated store holding the given number of reverse zones.
func benchStore(b *testing.B, zones int) *sqlite.Store {
	b.Helper()

	ctx := context.Background()
	s, err := sqlite.Open(ctx, sqlite.Options{Path: filepath.Join(b.TempDir(), "weg.db")})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := s.Close(); err != nil {
			b.Errorf("Close: %v", err)
		}
	})
	if err := s.Migrate(ctx); err != nil {
		b.Fatal(err)
	}

	soa := zone.DefaultSOA(zone.MustParseName("ns1.example.com."), zone.MustParseName("h.example.com."))
	if err := s.Update(ctx, func(tx store.Tx) error {
		for i := range zones {
			// A spread of /24 networks under 10.0.0.0/8, plus the one the
			// benchmark looks for.
			apex := fmt.Sprintf("%d.%d.10.in-addr.arpa.", i%256, i/256)
			z, zerr := zone.NewZone(zone.MustParseName(apex), soa)
			if zerr != nil {
				return zerr
			}
			z.ID = zone.ZoneID(id.New())
			if zerr := tx.CreateZone(ctx, &z); zerr != nil {
				return zerr
			}
		}
		return nil
	}); err != nil {
		b.Fatal(err)
	}
	return s
}
