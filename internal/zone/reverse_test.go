package zone_test

import (
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

func TestReverseName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		addr string
		want string
	}{
		{"ipv4", "192.0.2.10", "10.2.0.192.in-addr.arpa."},
		{"ipv4 zero", "0.0.0.0", "0.0.0.0.in-addr.arpa."},
		{"ipv4 broadcast", "255.255.255.255", "255.255.255.255.in-addr.arpa."},
		{"ipv4 loopback", "127.0.0.1", "1.0.0.127.in-addr.arpa."},
		{
			// RFC 3596 §2.5: one nibble per label, least significant first.
			name: "ipv6",
			addr: "2001:db8::1",
			want: "1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0." +
				"0.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.ip6.arpa.",
		},
		{
			name: "ipv6 unspecified",
			addr: "::",
			want: strings.Repeat("0.", 32) + "ip6.arpa.",
		},
		{
			name: "ipv6 with high nibbles",
			addr: "fe80::abcd",
			want: "d.c.b.a.0.0.0.0.0.0.0.0.0.0.0.0." +
				"0.0.0.0.0.0.0.0.0.0.0.0.0.8.e.f.ip6.arpa.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := zone.ReverseName(netip.MustParseAddr(tc.addr))
			if err != nil {
				t.Fatalf("ReverseName(%s): %v", tc.addr, err)
			}
			if got.String() != tc.want {
				t.Errorf("ReverseName(%s) = %q, want %q", tc.addr, got, tc.want)
			}
			if !zone.IsReverseName(got) {
				t.Errorf("%q should be recognised as a reverse name", got)
			}
		})
	}

	t.Run("invalid address", func(t *testing.T) {
		t.Parallel()

		if _, err := zone.ReverseName(netip.Addr{}); !errors.Is(err, zone.ErrInvalid) {
			t.Errorf("error = %v, want ErrInvalid", err)
		}
	})
}

func TestReverseZoneName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		prefix string
		want   string
	}{
		{"slash 24", "192.0.2.0/24", "2.0.192.in-addr.arpa."},
		{"slash 16", "192.0.0.0/16", "0.192.in-addr.arpa."},
		{"slash 8", "10.0.0.0/8", "10.in-addr.arpa."},
		{"slash 0", "0.0.0.0/0", "in-addr.arpa."},
		{"slash 32", "192.0.2.10/32", "10.2.0.192.in-addr.arpa."},

		// RFC 2317 §4: a prefix that stops inside an octet.
		{"rfc2317 slash 25", "192.0.2.0/25", "0/25.2.0.192.in-addr.arpa."},
		{"rfc2317 upper half", "192.0.2.128/25", "128/25.2.0.192.in-addr.arpa."},
		{"rfc2317 slash 26", "192.0.2.64/26", "64/26.2.0.192.in-addr.arpa."},
		{"rfc2317 slash 30", "192.0.2.8/30", "8/30.2.0.192.in-addr.arpa."},

		{"ipv6 slash 32", "2001:db8::/32", "8.b.d.0.1.0.0.2.ip6.arpa."},
		{"ipv6 slash 48", "2001:db8:1::/48", "1.0.0.0.8.b.d.0.1.0.0.2.ip6.arpa."},
		{"ipv6 slash 0", "::/0", "ip6.arpa."},
		{"ipv6 slash 4", "f000::/4", "f.ip6.arpa."},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := zone.ReverseZoneName(netip.MustParsePrefix(tc.prefix))
			if err != nil {
				t.Fatalf("ReverseZoneName(%s): %v", tc.prefix, err)
			}
			if got.String() != tc.want {
				t.Errorf("ReverseZoneName(%s) = %q, want %q", tc.prefix, got, tc.want)
			}
		})
	}

	t.Run("an ipv6 prefix off a nibble boundary is not expressible", func(t *testing.T) {
		t.Parallel()

		_, err := zone.ReverseZoneName(netip.MustParsePrefix("2001:db8::/33"))
		if !errors.Is(err, zone.ErrInvalid) {
			t.Fatalf("error = %v, want ErrInvalid", err)
		}
		// The error has to say what to do instead, not merely that it failed.
		if !strings.Contains(err.Error(), "/32") || !strings.Contains(err.Error(), "/36") {
			t.Errorf("error should suggest the neighbouring boundaries: %v", err)
		}
	})
}

func TestParseReversePrefix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"slash 24", "2.0.192.in-addr.arpa.", "192.0.2.0/24"},
		{"slash 16", "0.192.in-addr.arpa.", "192.0.0.0/16"},
		{"slash 8", "10.in-addr.arpa.", "10.0.0.0/8"},
		{"the whole namespace", "in-addr.arpa.", "0.0.0.0/0"},
		{"single host", "10.2.0.192.in-addr.arpa.", "192.0.2.10/32"},

		// RFC 2317 §4, the spelling the RFC itself uses.
		{"rfc2317 slash form", "0/25.2.0.192.in-addr.arpa.", "192.0.2.0/25"},
		{"rfc2317 upper half", "128/25.2.0.192.in-addr.arpa.", "192.0.2.128/25"},
		{"rfc2317 slash 26", "64/26.2.0.192.in-addr.arpa.", "192.0.2.64/26"},

		// The range spelling that BIND setups commonly use.
		{"rfc2317 range form", "0-127.2.0.192.in-addr.arpa.", "192.0.2.0/25"},
		{"rfc2317 range upper half", "128-255.2.0.192.in-addr.arpa.", "192.0.2.128/25"},
		{"rfc2317 range quarter", "64-127.2.0.192.in-addr.arpa.", "192.0.2.64/26"},
		{"rfc2317 range single", "5-5.2.0.192.in-addr.arpa.", "192.0.2.5/32"},

		{"ipv6 slash 32", "8.b.d.0.1.0.0.2.ip6.arpa.", "2001:db8::/32"},
		{"ipv6 slash 48", "1.0.0.0.8.b.d.0.1.0.0.2.ip6.arpa.", "2001:db8:1::/48"},
		{"ipv6 whole namespace", "ip6.arpa.", "::/0"},
		{"ipv6 uppercase digit is folded by the name type", "F.ip6.arpa.", "f000::/4"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := zone.ParseReversePrefix(zone.MustParseName(tc.in))
			if err != nil {
				t.Fatalf("ParseReversePrefix(%q): %v", tc.in, err)
			}
			if got != netip.MustParsePrefix(tc.want) {
				t.Errorf("ParseReversePrefix(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseReversePrefixRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		wantErr error
	}{
		{"forward name", "example.com.", zone.ErrNotReverse},
		{"arpa itself", "arpa.", zone.ErrNotReverse},
		{"the root", ".", zone.ErrNotReverse},

		{"octet above 255", "256.0.192.in-addr.arpa.", zone.ErrInvalid},
		{"not a number", "x.0.192.in-addr.arpa.", zone.ErrInvalid},
		{"too many octets", "1.2.3.4.5.in-addr.arpa.", zone.ErrInvalid},
		// A leading zero would let one network have two names.
		{"padded octet", "010.0.192.in-addr.arpa.", zone.ErrInvalid},

		{"classless prefix outside its octet", "0/24.2.0.192.in-addr.arpa.", zone.ErrInvalid},
		{"classless prefix beyond 32", "0/33.2.0.192.in-addr.arpa.", zone.ErrInvalid},
		{"classless label above another octet", "2.0/25.0.192.in-addr.arpa.", zone.ErrInvalid},
		{"classless range not a power of two", "0-99.2.0.192.in-addr.arpa.", zone.ErrInvalid},
		{"classless range not aligned", "1-128.2.0.192.in-addr.arpa.", zone.ErrInvalid},
		{"classless range reversed", "127-0.2.0.192.in-addr.arpa.", zone.ErrInvalid},
		{"classless label is nonsense", "0/x.2.0.192.in-addr.arpa.", zone.ErrInvalid},

		{"nibble of two digits", "ab.ip6.arpa.", zone.ErrInvalid},
		{"not a hex digit", "z.ip6.arpa.", zone.ErrInvalid},
		{"too many nibbles", strings.Repeat("0.", 33) + "ip6.arpa.", zone.ErrInvalid},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := zone.ParseReversePrefix(zone.MustParseName(tc.in))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ParseReversePrefix(%q) = %v, %v; want %v", tc.in, got, err, tc.wantErr)
			}
		})
	}
}

// TestReverseRoundTrip pins the two directions against each other: the zone
// name of a network must parse back to that network, and the PTR name of an
// address must sit inside it.
func TestReverseRoundTrip(t *testing.T) {
	t.Parallel()

	prefixes := []string{
		"0.0.0.0/0", "10.0.0.0/8", "192.0.0.0/16", "192.0.2.0/24", "192.0.2.10/32",
		"192.0.2.0/25", "192.0.2.128/25", "192.0.2.64/26", "192.0.2.8/29", "192.0.2.7/32",
		"::/0", "2001:db8::/32", "2001:db8:1::/48", "2001:db8::/64", "f000::/4",
	}

	for _, s := range prefixes {
		t.Run(s, func(t *testing.T) {
			t.Parallel()

			want := netip.MustParsePrefix(s)

			name, err := zone.ReverseZoneName(want)
			if err != nil {
				t.Fatalf("ReverseZoneName(%v): %v", want, err)
			}
			got, err := zone.ParseReversePrefix(name)
			if err != nil {
				t.Fatalf("ParseReversePrefix(%q): %v", name, err)
			}
			if got != want {
				t.Fatalf("%v became %q and parsed back as %v", want, name, got)
			}

			// The name of an address inside the network has to fall under the
			// zone that covers it, which is what makes PTR placement work.
			addr := want.Addr()
			ptr, err := zone.ReverseName(addr)
			if err != nil {
				t.Fatalf("ReverseName(%v): %v", addr, err)
			}
			// A classless zone name is not an ancestor of the plain PTR name;
			// that is the whole reason RFC 2317 needs CNAMEs in the parent.
			if want.Bits()%8 == 0 || !addr.Is4() {
				if !ptr.IsSubDomainOf(name) {
					t.Errorf("%q is not under the zone %q that covers %v", ptr, name, addr)
				}
			}
		})
	}
}

// TestReverseNameAddressesRoundTrip walks a range of addresses through the PTR
// name and back out of it, via the /32 or /128 zone form.
func TestReverseNameAddressesRoundTrip(t *testing.T) {
	t.Parallel()

	addrs := []string{
		"0.0.0.0", "1.2.3.4", "127.0.0.1", "192.0.2.10", "255.255.255.255",
		"::", "::1", "2001:db8::1", "fe80::abcd", "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff",
	}

	for _, s := range addrs {
		t.Run(s, func(t *testing.T) {
			t.Parallel()

			addr := netip.MustParseAddr(s)
			name, err := zone.ReverseName(addr)
			if err != nil {
				t.Fatalf("ReverseName(%v): %v", addr, err)
			}

			got, err := zone.ParseReversePrefix(name)
			if err != nil {
				t.Fatalf("ParseReversePrefix(%q): %v", name, err)
			}
			if got.Addr() != addr {
				t.Errorf("%v became %q and parsed back as %v", addr, name, got.Addr())
			}
			if want := addr.BitLen(); got.Bits() != want {
				t.Errorf("the PTR name of a single address should be a /%d, got /%d", want, got.Bits())
			}
		})
	}
}

func TestZoneReverseOwner(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		apex    string
		addr    string
		want    string
		wantErr bool
	}{
		// An ordinary reverse zone is an ancestor of every name below it.
		{"ipv4 /24", "2.0.192.in-addr.arpa.", "192.0.2.10", "10.2.0.192.in-addr.arpa.", false},
		{"ipv4 /16", "0.192.in-addr.arpa.", "192.0.2.10", "10.2.0.192.in-addr.arpa.", false},
		{"ipv4 /8", "10.in-addr.arpa.", "10.1.2.3", "3.2.1.10.in-addr.arpa.", false},
		{"ipv6 nibble", "8.b.d.0.1.0.0.2.ip6.arpa.", "2001:db8::1",
			"1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.ip6.arpa.", false},

		// RFC 2317 §4: the classless child is not an ancestor of the plain
		// reverse name, so the host part is re-attached under its apex. This is
		// the name the parent's generated CNAME has to point at.
		{"classless slash form", "0/25.2.0.192.in-addr.arpa.", "192.0.2.10",
			"10.0/25.2.0.192.in-addr.arpa.", false},
		{"classless range form", "0-127.2.0.192.in-addr.arpa.", "192.0.2.10",
			"10.0-127.2.0.192.in-addr.arpa.", false},
		{"classless upper half", "128/25.2.0.192.in-addr.arpa.", "192.0.2.200",
			"200.128/25.2.0.192.in-addr.arpa.", false},

		// An address the zone does not answer for has no owner name in it.
		{"outside the network", "2.0.192.in-addr.arpa.", "192.0.3.10", "", true},
		{"outside the classless block", "0/25.2.0.192.in-addr.arpa.", "192.0.2.200", "", true},
		{"wrong family", "2.0.192.in-addr.arpa.", "2001:db8::1", "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			z, err := zone.NewZone(zone.MustParseName(tc.apex), testSOA())
			if err != nil {
				t.Fatalf("NewZone(%q): %v", tc.apex, err)
			}

			got, err := z.ReverseOwner(netip.MustParseAddr(tc.addr))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ReverseOwner(%s) = %q, want an error", tc.addr, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReverseOwner(%s): %v", tc.addr, err)
			}
			if got.String() != tc.want {
				t.Errorf("ReverseOwner(%s) = %q, want %q", tc.addr, got, tc.want)
			}
			// Whatever comes back has to be a name the zone actually holds, or
			// the PTR would be written somewhere no query reaches.
			if !got.IsSubDomainOf(z.Name) {
				t.Errorf("%q is not inside the zone %q", got, z.Name)
			}
		})
	}

	t.Run("a forward zone has no reverse owner", func(t *testing.T) {
		t.Parallel()

		z, err := zone.NewZone(zone.MustParseName("example.com."), testSOA())
		if err != nil {
			t.Fatalf("NewZone: %v", err)
		}
		if _, err := z.ReverseOwner(netip.MustParseAddr("192.0.2.10")); err == nil {
			t.Error("a forward zone produced a reverse owner name")
		}
	})
}
