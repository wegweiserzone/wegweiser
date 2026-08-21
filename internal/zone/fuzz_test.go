package zone_test

import (
	"net/netip"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// FuzzParseName checks that no input makes the presentation parser panic, and
// that every name it accepts survives a trip through its own printed form.
// Failing that round trip would mean a zonefile export could not be imported
// back, which is the migration story the project rests on.
func FuzzParseName(f *testing.F) {
	seeds := []string{
		".",
		"example.com.",
		"www.example.com",
		"*.example.com.",
		"_dmarc.example.com.",
		"0/25.2.0.192.in-addr.arpa.",
		"8.b.d.0.1.0.0.2.ip6.arpa.",
		`a\.b.example.com.`,
		`a\\b.example.com.`,
		`\000.example.com.`,
		`\255.example.com.`,
		`\065.example.com.`,
		"WWW.EXAMPLE.COM.",
		"..",
		"",
		`\`,
		`\12`,
		`\256.example.com.`,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, s string) {
		n, err := zone.ParseName(s)
		if err != nil {
			return
		}

		checkInvariants(t, n)

		printed := n.String()
		back, err := zone.ParseName(printed)
		if err != nil {
			t.Fatalf("ParseName(%q) printed %q, which does not parse: %v", s, printed, err)
		}
		if !back.Equal(n) {
			t.Fatalf("round trip changed the name: %q printed %q parsed back as %q",
				s, printed, back.String())
		}
		if back.String() != printed {
			t.Fatalf("printing is not stable: %q then %q", printed, back.String())
		}
	})
}

// FuzzNameFromWire checks the wire decoder against the same invariants. A
// malformed packet reaches this function directly, so it must never panic.
func FuzzNameFromWire(f *testing.F) {
	seeds := [][]byte{
		{0},
		{3, 'w', 'w', 'w', 0},
		{7, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 3, 'c', 'o', 'm', 0},
		{1, '*', 7, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 0},
		{0xC0, 0x0C},     // compression pointer
		{0x41, 'a', 0},   // reserved label type
		{64, 'a', 0},     // label length above the limit
		{5, 'a', 'b', 0}, // label overruns the buffer
		{1, 'a', 0, 'x'}, // trailing octets
		{1, 'a'},         // unterminated
		{1, 0x00, 0},     // zero octet inside a label, which is legal
		{},
	}
	for _, b := range seeds {
		f.Add(b)
	}

	f.Fuzz(func(t *testing.T, b []byte) {
		n, err := zone.NameFromWire(b)
		if err != nil {
			return
		}

		checkInvariants(t, n)

		// Re-decoding the encoding must be a fixed point. It is not required
		// to equal the input, which may have carried uppercase letters.
		again, err := zone.NameFromWire(n.Wire())
		if err != nil {
			t.Fatalf("re-decoding own wire form of %q failed: %v", n, err)
		}
		if !again.Equal(n) {
			t.Fatalf("wire round trip changed the name: %q became %q", n, again)
		}

		printed, err := zone.ParseName(n.String())
		if err != nil {
			t.Fatalf("printed form %q of a wire name does not parse: %v", n.String(), err)
		}
		if !printed.Equal(n) {
			t.Fatalf("presentation round trip changed the name: %q became %q", n, printed)
		}
	})
}

// checkInvariants asserts the properties every valid Name must satisfy,
// whichever parser produced it.
func checkInvariants(t *testing.T, n zone.Name) {
	t.Helper()

	if n.IsZero() {
		t.Fatal("a successfully parsed name must not be the zero value")
	}
	if n.WireLen() > zone.MaxNameWireLen {
		t.Fatalf("WireLen() = %d, above the limit of %d", n.WireLen(), zone.MaxNameWireLen)
	}

	labels := n.Labels()
	if len(labels) != n.LabelCount() {
		t.Fatalf("LabelCount() = %d but Labels() returned %d", n.LabelCount(), len(labels))
	}
	for _, l := range labels {
		if l == "" {
			t.Fatal("a label must not be empty")
		}
		if len(l) > zone.MaxLabelLen {
			t.Fatalf("label %q is %d octets, above the limit of %d", l, len(l), zone.MaxLabelLen)
		}
	}

	// Reflexivity is checked against an independently rebuilt copy rather than
	// against n itself, so the comparison actually goes through the encoding.
	same, err := zone.NameFromWire(n.Wire())
	if err != nil {
		t.Fatalf("rebuilding %q from its own wire form failed: %v", n, err)
	}
	if n.Compare(same) != 0 {
		t.Fatalf("%q does not compare equal to a rebuilt copy of itself", n)
	}
	if !n.IsSubDomainOf(same) {
		t.Fatalf("%q is not a subdomain of a rebuilt copy of itself", n)
	}
	if !n.IsSubDomainOf(zone.Root) {
		t.Fatalf("%q is not a subdomain of the root", n)
	}

	// Walking to the root must terminate, and every step must stay below the
	// name it came from.
	prev := n
	for steps := 0; ; steps++ {
		p, ok := prev.Parent()
		if !ok {
			break
		}
		if !prev.IsSubDomainOf(p) {
			t.Fatalf("%q is not a subdomain of its parent %q", prev, p)
		}
		if steps > zone.MaxNameWireLen {
			t.Fatalf("walking up from %q does not terminate", n)
		}
		prev = p
	}
	if !prev.IsRoot() {
		t.Fatalf("walking up from %q ended at %q, not the root", n, prev)
	}
}

// FuzzParseRData checks the property ADR 0001 rests on: canonicalisation is a
// fixed point. If parsing the canonical form of some data produced anything
// other than that same form, the value written to the database would differ
// from the value a re-import produces, and the unique index that catches
// duplicate records inside an RRset would stop working.
func FuzzParseRData(f *testing.F) {
	seeds := []struct {
		typ uint16
		in  string
	}{
		{1, "192.0.2.1"},
		{28, "2001:DB8::1"},
		{5, "Foo.Example.COM."},
		{5, `\065.example.com.`},
		{2, "ns1.example.com."},
		{12, "www.example.com."},
		{15, "10   mail.example.com."},
		{16, "hello"},
		{16, `"a" "b"`},
		{33, "10 20 8080 host.example.com."},
		{6, "ns.example.com. hostmaster.example.com. 1 2 3 4 5"},
		{257, `0 issue "letsencrypt.org"`},
		{65280, `\# 4 0A0B0C0D`},
		{65280, `\# 0`},
		{1, ""},
		{5, "relative"},
		{1, "192.0.2.1\nevil. 0 IN A 192.0.2.2"},
	}
	for _, s := range seeds {
		f.Add(s.typ, s.in)
	}

	f.Fuzz(func(t *testing.T, typ uint16, in string) {
		rt := zone.RRType(typ)

		first, err := zone.ParseRData(rt, zone.ClassIN, in)
		if err != nil {
			return
		}
		if first.IsZero() {
			t.Fatalf("ParseRData(%s, %q) succeeded but produced empty data", rt, in)
		}

		second, err := zone.ParseRData(rt, zone.ClassIN, first.String())
		if err != nil {
			t.Fatalf("the canonical form %q of (%s, %q) does not parse: %v",
				first.String(), rt, in, err)
		}
		if !second.Equal(first) {
			t.Fatalf("canonicalisation is not a fixed point for (%s, %q): %q then %q",
				rt, in, first.String(), second.String())
		}

		// An address must agree with the family its record type implies, since
		// the store writes four octets for A and sixteen for AAAA.
		if addr, ok := first.Address(rt); ok {
			if (rt == zone.TypeA) != addr.Is4() {
				t.Fatalf("%s carried an address of the wrong family: %v", rt, addr)
			}
		}
	})
}

// FuzzReverseRoundTrip checks that the two directions of the reverse mapping
// agree for every network they can express. Reverse automation places a PTR by
// deriving a name from an address and finding the zone whose name covers it, so
// a disagreement here would file a record under the wrong zone.
func FuzzReverseRoundTrip(f *testing.F) {
	f.Add([]byte{192, 0, 2, 0}, 24)
	f.Add([]byte{192, 0, 2, 0}, 25)
	f.Add([]byte{10, 0, 0, 0}, 8)
	f.Add([]byte{0, 0, 0, 0}, 0)
	f.Add([]byte{255, 255, 255, 255}, 32)
	f.Add(make([]byte, 16), 0)
	f.Add([]byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, 32)

	f.Fuzz(func(t *testing.T, raw []byte, bits int) {
		addr, ok := netip.AddrFromSlice(raw)
		if !ok {
			return
		}
		if bits < 0 || bits > addr.BitLen() {
			return
		}

		want := netip.PrefixFrom(addr, bits).Masked()

		name, err := zone.ReverseZoneName(want)
		if err != nil {
			// Only an IPv6 prefix off a nibble boundary may be refused.
			if addr.Is4() || bits%4 == 0 {
				t.Fatalf("ReverseZoneName(%v) refused an expressible prefix: %v", want, err)
			}
			return
		}
		if !zone.IsReverseName(name) {
			t.Fatalf("ReverseZoneName(%v) produced %q, which is not a reverse name", want, name)
		}

		got, err := zone.ParseReversePrefix(name)
		if err != nil {
			t.Fatalf("ReverseZoneName(%v) produced %q, which does not parse: %v", want, name, err)
		}
		if got != want {
			t.Fatalf("%v became %q and parsed back as %v", want, name, got)
		}
	})
}

// FuzzParseReversePrefix checks that no name makes the reverse parser panic,
// and that anything it accepts is a network its own printer agrees with.
func FuzzParseReversePrefix(f *testing.F) {
	seeds := []string{
		"2.0.192.in-addr.arpa.",
		"0/25.2.0.192.in-addr.arpa.",
		"0-127.2.0.192.in-addr.arpa.",
		"8.b.d.0.1.0.0.2.ip6.arpa.",
		"in-addr.arpa.",
		"ip6.arpa.",
		"example.com.",
		"1.2.3.4.5.in-addr.arpa.",
		"0/x.2.0.192.in-addr.arpa.",
		"999-1.2.0.192.in-addr.arpa.",
		"ab.ip6.arpa.",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, s string) {
		n, err := zone.ParseName(s)
		if err != nil {
			return
		}

		prefix, err := zone.ParseReversePrefix(n)
		if err != nil {
			return
		}
		if !prefix.IsValid() {
			t.Fatalf("%q parsed to an invalid prefix", n)
		}
		if prefix != prefix.Masked() {
			t.Fatalf("%q parsed to %v, which has bits set below its prefix length", n, prefix)
		}

		// Whatever the parser accepts, the printer must be able to name, and
		// that name must mean the same network.
		back, err := zone.ReverseZoneName(prefix)
		if err != nil {
			t.Fatalf("%q parsed to %v, which cannot be named: %v", n, prefix, err)
		}
		again, err := zone.ParseReversePrefix(back)
		if err != nil {
			t.Fatalf("%v was named %q, which does not parse: %v", prefix, back, err)
		}
		if again != prefix {
			t.Fatalf("%v was named %q and parsed back as %v", prefix, back, again)
		}
	})
}
