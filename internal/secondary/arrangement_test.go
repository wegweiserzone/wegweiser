package secondary_test

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/secondary"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

func addr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return a
}

func prefix(t *testing.T, s string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return p
}

func TestWarnings(t *testing.T) {
	keyName := "ns2.example.com."
	withKey := func(t *testing.T) secondary.Config {
		t.Helper()
		return secondary.Config{
			Primary: addrPort(t, "192.0.2.1:53"),
			Zones:   []zone.Name{name(t, "example.com.")},
			Key: &secondary.Key{
				Name: name(t, keyName), Algorithm: zone.HMACSHA256, Secret: "eA==",
			},
		}
	}
	open := func(t *testing.T) secondary.Config {
		t.Helper()
		return secondary.Config{
			Primary: addrPort(t, "192.0.2.1:53"),
			Zones:   []zone.Name{name(t, "example.com.")},
		}
	}

	cases := []struct {
		name        string
		config      func(*testing.T) secondary.Config
		arrangement func(*testing.T) secondary.Arrangement
		want        []string
	}{
		{
			name:   "a server that grants nothing",
			config: open,
			arrangement: func(t *testing.T) secondary.Arrangement {
				t.Helper()
				return secondary.Arrangement{Notify: []netip.Addr{addr(t, "198.51.100.53")}}
			},
			want: []string{"nobody may transfer"},
		},
		{
			name:   "a key that was created and never granted anything",
			config: withKey,
			arrangement: func(t *testing.T) secondary.Arrangement {
				t.Helper()
				return secondary.Arrangement{
					AllowPrefixes: []netip.Prefix{prefix(t, "203.0.113.0/24")},
					Notify:        []netip.Addr{addr(t, "198.51.100.53")},
				}
			},
			want: []string{"the key " + keyName + " is not on the transfer list"},
		},
		{
			name:   "an address nothing on either list names",
			config: open,
			arrangement: func(t *testing.T) secondary.Arrangement {
				t.Helper()
				return secondary.Arrangement{
					AllowPrefixes: []netip.Prefix{prefix(t, "203.0.113.0/24")},
					Notify:        []netip.Addr{addr(t, "203.0.113.53")},
					Secondary:     addr(t, "198.51.100.53"),
				}
			},
			want: []string{
				"198.51.100.53 is not on the transfer list",
				"198.51.100.53 is not on the notify list",
			},
		},
		{
			name:   "transfers granted and nobody told",
			config: withKey,
			arrangement: func(t *testing.T) secondary.Arrangement {
				t.Helper()
				return secondary.Arrangement{AllowKeys: []zone.Name{name(t, keyName)}}
			},
			want: []string{"nobody is told when a zone changes"},
		},
		{
			// The key grants from any address, so where one is used the
			// transfer list has nothing to say about where the request came
			// from.
			name:   "a key on the list, from an address that is not",
			config: withKey,
			arrangement: func(t *testing.T) secondary.Arrangement {
				t.Helper()
				return secondary.Arrangement{
					AllowKeys: []zone.Name{name(t, keyName)},
					Notify:    []netip.Addr{addr(t, "198.51.100.53")},
					Secondary: addr(t, "198.51.100.53"),
				}
			},
		},
		{
			name:   "a prefix that covers the secondary",
			config: open,
			arrangement: func(t *testing.T) secondary.Arrangement {
				t.Helper()
				return secondary.Arrangement{
					AllowPrefixes: []netip.Prefix{prefix(t, "198.51.100.0/24")},
					Notify:        []netip.Addr{addr(t, "198.51.100.53")},
					Secondary:     addr(t, "198.51.100.53"),
				}
			},
		},
		{
			// Nobody named the far end, so neither list can be checked against
			// anything and saying so would be guessing.
			name:   "an arrangement described but not pointed anywhere",
			config: open,
			arrangement: func(t *testing.T) secondary.Arrangement {
				t.Helper()
				return secondary.Arrangement{
					AllowPrefixes: []netip.Prefix{prefix(t, "203.0.113.0/24")},
					Notify:        []netip.Addr{addr(t, "203.0.113.53")},
				}
			},
		},
		{
			name: "a server with nothing to mirror",
			config: func(t *testing.T) secondary.Config {
				t.Helper()
				c := open(t)
				c.Zones = nil
				return c
			},
			arrangement: func(t *testing.T) secondary.Arrangement {
				t.Helper()
				return secondary.Arrangement{
					AllowPrefixes: []netip.Prefix{prefix(t, "198.51.100.0/24")},
					Notify:        []netip.Addr{addr(t, "198.51.100.53")},
				}
			},
			want: []string{"nothing to mirror"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.arrangement(t).Warnings(c.config(t))
			if len(got) != len(c.want) {
				t.Fatalf("wanted %d warnings, got %d:\n%s",
					len(c.want), len(got), strings.Join(got, "\n"))
			}
			for i, want := range c.want {
				if !strings.Contains(got[i], want) {
					t.Errorf("warning %d does not say %q:\n%s", i, want, got[i])
				}
			}
		})
	}
}

// TestWarningsMatchesAMappedAddress covers a notify list that came back holding
// v4-in-v6 addresses, which must not read as a different secondary.
func TestWarningsMatchesAMappedAddress(t *testing.T) {
	a := secondary.Arrangement{
		AllowPrefixes: []netip.Prefix{prefix(t, "198.51.100.0/24")},
		Notify:        []netip.Addr{netip.AddrFrom16(addr(t, "198.51.100.53").As16())},
		Secondary:     addr(t, "198.51.100.53"),
	}
	c := secondary.Config{
		Primary: addrPort(t, "192.0.2.1:53"),
		Zones:   []zone.Name{name(t, "example.com.")},
	}
	if got := a.Warnings(c); len(got) != 0 {
		t.Errorf("a mapped address reads as a different one:\n%s", strings.Join(got, "\n"))
	}
}
