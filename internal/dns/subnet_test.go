package dns

import (
	"net"
	"net/netip"
	"slices"
	"sync/atomic"
	"testing"

	wire "github.com/miekg/dns"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// withSubnet gives a query that already carries an OPT record the client
// subnet option a resolver sends (RFC 7871 §6).
func withSubnet(family uint16, source uint8, addr string) func(*wire.Msg) {
	return func(m *wire.Msg) {
		opt := m.IsEdns0()
		opt.Option = append(opt.Option, &wire.EDNS0_SUBNET{
			Code: wire.EDNS0SUBNET, Family: family, SourceNetmask: source,
			Address: net.ParseIP(addr),
		})
	}
}

// withRawSubnet gives a query an option with the client subnet code and
// whatever octets are handed over, which is the only way to send one a
// careful client never would.
func withRawSubnet(data ...byte) func(*wire.Msg) {
	return func(m *wire.Msg) {
		opt := m.IsEdns0()
		opt.Option = append(opt.Option, &wire.EDNS0_LOCAL{Code: wire.EDNS0SUBNET, Data: data})
	}
}

// responseSubnet returns the client subnet option of a response, or nil.
func responseSubnet(m *wire.Msg) *wire.EDNS0_SUBNET {
	if opt := m.IsEdns0(); opt != nil {
		return requestSubnet(opt)
	}
	return nil
}

// believing returns a responder that takes the word of resolvers in these
// prefixes, the way a server wires one up.
func believing(prefixes ...string) *Responder {
	r := NewResponder(DefaultLimits())
	var list atomic.Pointer[resolverHolder]
	h := &resolverHolder{}
	for _, p := range prefixes {
		h.prefixes = append(h.prefixes, netip.MustParsePrefix(p))
	}
	list.Store(h)
	r.resolvers = &list
	return r
}

// D50, RFC 7871 §7.2.1: an authority that supports the option sends it back
// with the family, source prefix and address the query had, and a scope of 0
// where the answer is the same for everybody, which here it always is.
func TestSubnetIsSentBackWithScopeZero(t *testing.T) {
	t.Parallel()
	snap := resolveFixture(t)

	plain, _ := respond(t, NewResponder(DefaultLimits()), snap,
		packQuery(t, "www.example.com.", zone.TypeA, withEDNS(1232, 0)), UDP)

	for _, tc := range []struct {
		name   string
		family uint16
		source uint8
		addr   string
	}{
		{"an IPv4 network", 1, 24, "198.51.100.0"},
		{"a whole IPv4 address", 1, 32, "198.51.100.7"},
		{"an IPv6 network", 2, 56, "2001:db8:1234:5600::"},
		{"a client that asked not to be named", 1, 0, "0.0.0.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := respond(t, NewResponder(DefaultLimits()), snap,
				packQuery(t, "www.example.com.", zone.TypeA,
					withEDNS(1232, 0), withSubnet(tc.family, tc.source, tc.addr)), UDP)

			if got.Rcode != wire.RcodeSuccess {
				t.Fatalf("rcode = %s, want NOERROR", wire.RcodeToString[got.Rcode])
			}
			back := responseSubnet(got)
			if back == nil {
				t.Fatal("the response carries no client subnet option, and the query did")
			}
			if back.Family != tc.family || back.SourceNetmask != tc.source || back.SourceScope != 0 {
				t.Errorf("sent back family %d, source /%d, scope /%d; want %d, /%d, /0",
					back.Family, back.SourceNetmask, back.SourceScope, tc.family, tc.source)
			}
			if tc.source > 0 && !back.Address.Equal(net.ParseIP(tc.addr)) {
				t.Errorf("sent back address %s, want %s", back.Address, tc.addr)
			}
			// Nothing about the answer depends on whom it is for.
			if a, b := lines(got.Answer), lines(plain.Answer); !slices.Equal(a, b) {
				t.Errorf("the answer changed with the option:\n got %q\nwant %q", a, b)
			}
		})
	}

	t.Run("a query without the option gets none", func(t *testing.T) {
		if responseSubnet(plain) != nil {
			t.Error("the response carries a client subnet option the query did not")
		}
	})

	t.Run("a referral carries scope 0 too", func(t *testing.T) {
		got, _ := respond(t, NewResponder(DefaultLimits()), snap,
			packQuery(t, "host.sub2.example.com.", zone.TypeA,
				withEDNS(1232, 0), withSubnet(1, 24, "198.51.100.0")), UDP)
		if got.Authoritative || len(got.Answer) != 0 || len(got.Ns) == 0 {
			t.Fatalf("this is no referral: AA %v, %d answers, %d in authority; the fixture moved",
				got.Authoritative, len(got.Answer), len(got.Ns))
		}
		if back := responseSubnet(got); back == nil || back.SourceScope != 0 {
			t.Errorf("a delegation sent back %+v, want the option with scope 0 (RFC 7871 §7.4)", back)
		}
	})
}

// RFC 7871 §6 and §7.2.1: an option the server cannot read is the query's
// mistake, and is answered FORMERR rather than guessed at.
func TestSubnetMalformedIsFormErr(t *testing.T) {
	t.Parallel()
	snap := resolveFixture(t)

	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"an address longer than its prefix needs", []byte{0, 1, 24, 0, 198, 51, 100, 0}},
		{"an address shorter than its prefix needs", []byte{0, 1, 24, 0, 198, 51}},
		{"a prefix longer than the family allows", []byte{0, 1, 33, 0, 198, 51, 100, 7, 0}},
		{"a family nobody defined", []byte{0, 3, 8, 0, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := respond(t, NewResponder(DefaultLimits()), snap,
				packQuery(t, "www.example.com.", zone.TypeA, withEDNS(1232, 0), withRawSubnet(tc.data...)), UDP)
			if got.Rcode != wire.RcodeFormatError {
				t.Errorf("rcode = %s, want FORMERR", wire.RcodeToString[got.Rcode])
			}
			if len(got.Answer) != 0 {
				t.Errorf("a malformed query was answered: %v", got.Answer)
			}
		})
	}
}

// D50: what the option says about the client is believed from a resolver on
// the list and from nobody else, and is what a watcher is shown.
func TestSubnetNamesTheClientOnlyForABelievedResolver(t *testing.T) {
	t.Parallel()
	snap := resolveFixture(t)

	for _, tc := range []struct {
		name    string
		r       *Responder
		source  uint8
		addr    string
		wantFor string
	}{
		{"a listed resolver naming a host", believing("192.0.2.1/32"), 32, "198.51.100.7", "198.51.100.7/32"},
		{"a listed network naming a network", believing("192.0.2.0/24"), 24, "198.51.100.0", "198.51.100.0/24"},
		{"a resolver nobody listed", believing("203.0.113.0/24"), 32, "198.51.100.7", ""},
		{"no list at all", NewResponder(DefaultLimits()), 32, "198.51.100.7", ""},
		{"a client that asked not to be named", believing("192.0.2.1/32"), 0, "0.0.0.0", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			respond(t, tc.r, snap, packQuery(t, "www.example.com.", zone.TypeA,
				withEDNS(1232, 0), withSubnet(1, tc.source, tc.addr)), UDP)

			got := tc.r.Observed().For
			switch {
			case tc.wantFor == "" && got.IsValid():
				t.Errorf("the client is shown as %s, want nobody", got)
			case tc.wantFor != "" && got != netip.MustParsePrefix(tc.wantFor):
				t.Errorf("the client is shown as %v, want %s", got, tc.wantFor)
			}
		})
	}

	// Written raw, because a client's own library masks before it sends.
	t.Run("bits a resolver left past its prefix are not believed", func(t *testing.T) {
		r := believing("192.0.2.1/32")
		respond(t, r, snap, packQuery(t, "www.example.com.", zone.TypeA,
			withEDNS(1232, 0), withRawSubnet(0, 1, 20, 0, 198, 51, 111)), UDP)
		if got, want := r.Observed().For, netip.MustParsePrefix("198.51.96.0/20"); got != want {
			t.Errorf("the client is shown as %v, want %s", got, want)
		}
	})

	t.Run("an exchange without the option names nobody, after one that did", func(t *testing.T) {
		r := believing("192.0.2.1/32")
		respond(t, r, snap, packQuery(t, "www.example.com.", zone.TypeA,
			withEDNS(1232, 0), withSubnet(1, 32, "198.51.100.7")), UDP)
		respond(t, r, snap, packQuery(t, "www.example.com.", zone.TypeA, withEDNS(1232, 0)), UDP)
		if got := r.Observed().For; got.IsValid() {
			t.Errorf("a reused responder still names %s", got)
		}
	})
}

// The walk that measures the option sits in the path a malformed packet
// reaches, so it is fed what a careless or hostile sender might write.
func FuzzSubnetOptionLength(f *testing.F) {
	f.Add([]byte{})
	f.Add(make([]byte, headerLen))
	m := new(wire.Msg)
	m.SetQuestion("www.example.com.", wire.TypeA)
	m.SetEdns0(1232, false)
	m.IsEdns0().Option = append(m.IsEdns0().Option, &wire.EDNS0_SUBNET{
		Code: wire.EDNS0SUBNET, Family: 1, SourceNetmask: 24, Address: net.ParseIP("198.51.100.0"),
	})
	if b, err := m.Pack(); err == nil {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, msg []byte) {
		n, ok := subnetOptionLength(msg)
		if ok && (n < 0 || n > len(msg)) {
			t.Fatalf("a length of %d out of a %d-octet message", n, len(msg))
		}
	})
}
