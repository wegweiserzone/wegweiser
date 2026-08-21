package zone_test

import (
	"errors"
	"net/netip"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

func newTestZone(t *testing.T, apex string) zone.Zone {
	t.Helper()

	z, err := zone.NewZone(zone.MustParseName(apex), testSOA())
	if err != nil {
		t.Fatalf("NewZone(%q): %v", apex, err)
	}
	return z
}

func TestNewZone(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		apex       string
		wantKind   zone.Kind
		wantPrefix string // empty means none
	}{
		{"forward", "example.com.", zone.KindForward, ""},
		{"forward under arpa but not reverse", "example.arpa.", zone.KindForward, ""},
		{"reverse ipv4", "2.0.192.in-addr.arpa.", zone.KindReverse, "192.0.2.0/24"},
		{"reverse ipv4 classless", "0/25.2.0.192.in-addr.arpa.", zone.KindReverse, "192.0.2.0/25"},
		{"reverse ipv6", "8.b.d.0.1.0.0.2.ip6.arpa.", zone.KindReverse, "2001:db8::/32"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			z, err := zone.NewZone(zone.MustParseName(tc.apex), testSOA())
			if err != nil {
				t.Fatalf("NewZone(%q): %v", tc.apex, err)
			}
			if z.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", z.Kind, tc.wantKind)
			}
			if tc.wantPrefix == "" {
				if z.Prefix.IsValid() {
					t.Errorf("Prefix = %v, want none", z.Prefix)
				}
				return
			}
			if z.Prefix != netip.MustParsePrefix(tc.wantPrefix) {
				t.Errorf("Prefix = %v, want %v", z.Prefix, tc.wantPrefix)
			}
		})
	}

	t.Run("an unreadable reverse name is refused at creation", func(t *testing.T) {
		t.Parallel()

		// Storing this would leave a zone the reverse automation has to skip
		// silently, which is worse than refusing it now.
		_, err := zone.NewZone(zone.MustParseName("nope.2.0.192.in-addr.arpa."), testSOA())
		if !errors.Is(err, zone.ErrInvalid) {
			t.Fatalf("error = %v, want ErrInvalid", err)
		}
	})

	t.Run("a broken SOA is refused", func(t *testing.T) {
		t.Parallel()

		soa := testSOA()
		soa.Refresh = 0
		if _, err := zone.NewZone(zone.MustParseName("example.com."), soa); !errors.Is(err, zone.ErrInvalid) {
			t.Fatalf("error = %v, want ErrInvalid", err)
		}
	})
}

func TestZoneValidateRejectsInconsistentKind(t *testing.T) {
	t.Parallel()

	t.Run("forward zone carrying a prefix", func(t *testing.T) {
		t.Parallel()

		z := newTestZone(t, "example.com.")
		z.Prefix = netip.MustParsePrefix("192.0.2.0/24")
		if err := z.Validate(); !errors.Is(err, zone.ErrInvalid) {
			t.Errorf("error = %v, want ErrInvalid", err)
		}
	})

	t.Run("reverse name marked forward", func(t *testing.T) {
		t.Parallel()

		z := newTestZone(t, "2.0.192.in-addr.arpa.")
		z.Kind = zone.KindForward
		z.Prefix = netip.Prefix{}
		if err := z.Validate(); !errors.Is(err, zone.ErrInvalid) {
			t.Errorf("error = %v, want ErrInvalid", err)
		}
	})

	t.Run("prefix disagreeing with the name", func(t *testing.T) {
		t.Parallel()

		z := newTestZone(t, "2.0.192.in-addr.arpa.")
		z.Prefix = netip.MustParsePrefix("10.0.0.0/8")
		if err := z.Validate(); !errors.Is(err, zone.ErrInvalid) {
			t.Errorf("error = %v, want ErrInvalid", err)
		}
	})

	t.Run("unknown kind", func(t *testing.T) {
		t.Parallel()

		z := newTestZone(t, "example.com.")
		z.Kind = "sideways"
		if err := z.Validate(); !errors.Is(err, zone.ErrInvalid) {
			t.Errorf("error = %v, want ErrInvalid", err)
		}
	})
}

func TestZoneContainsAndCovers(t *testing.T) {
	t.Parallel()

	fwd := newTestZone(t, "example.com.")
	rev := newTestZone(t, "2.0.192.in-addr.arpa.")

	t.Run("contains", func(t *testing.T) {
		t.Parallel()

		for name, want := range map[string]bool{
			"example.com.":     true,
			"www.example.com.": true,
			"a.b.example.com.": true,
			"example.net.":     false,
			"notexample.com.":  false,
			"com.":             false,
			"WWW.EXAMPLE.COM.": true,
		} {
			if got := fwd.Contains(zone.MustParseName(name)); got != want {
				t.Errorf("Contains(%q) = %v, want %v", name, got, want)
			}
		}
		if fwd.Contains(zone.Name{}) {
			t.Error("the zero name is in no zone")
		}
	})

	t.Run("apex", func(t *testing.T) {
		t.Parallel()

		if !fwd.IsApex(zone.MustParseName("example.com.")) {
			t.Error("the apex should be recognised")
		}
		if fwd.IsApex(zone.MustParseName("www.example.com.")) {
			t.Error("a subordinate name is not the apex")
		}
	})

	t.Run("covers", func(t *testing.T) {
		t.Parallel()

		for addr, want := range map[string]bool{
			"192.0.2.0":   true,
			"192.0.2.10":  true,
			"192.0.2.255": true,
			"192.0.3.1":   false,
			"10.0.0.1":    false,
			"2001:db8::1": false,
		} {
			if got := rev.Covers(netip.MustParseAddr(addr)); got != want {
				t.Errorf("Covers(%s) = %v, want %v", addr, got, want)
			}
		}
		if fwd.Covers(netip.MustParseAddr("192.0.2.10")) {
			t.Error("a forward zone covers no address")
		}
	})
}
