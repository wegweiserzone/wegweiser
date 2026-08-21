package zone_test

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

func TestParseRDataCanonical(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		typ  zone.RRType
		in   string
		want string
	}{
		{"a", zone.TypeA, "192.0.2.1", "192.0.2.1"},
		{"aaaa is compressed", zone.TypeAAAA, "2001:0db8:0000:0000:0000:0000:0000:0001", "2001:db8::1"},
		{"aaaa is lowercased", zone.TypeAAAA, "2001:DB8::1", "2001:db8::1"},
		{"surrounding space", zone.TypeA, "   192.0.2.1   ", "192.0.2.1"},
		{"inner space is collapsed", zone.TypeMX, "10     mail.example.com.", "10 mail.example.com."},
		{"tab separated", zone.TypeMX, "10\tmail.example.com.", "10 mail.example.com."},

		// RFC 4343: names are compared case-insensitively, so the stored form
		// folds. Anything else would let one name occupy two rows of an RRset.
		{"cname target is folded", zone.TypeCNAME, "Foo.Example.COM.", "foo.example.com."},
		{"mx target is folded", zone.TypeMX, "10 Mail.Example.COM.", "10 mail.example.com."},
		{"srv target is folded", zone.TypeSRV, "10 20 8080 Host.Example.COM.", "10 20 8080 host.example.com."},
		{"ns target is folded", zone.TypeNS, "NS1.Example.COM.", "ns1.example.com."},
		{"ptr target is folded", zone.TypePTR, "WWW.Example.COM.", "www.example.com."},

		// Character data is not a name and keeps its case.
		{"txt keeps case", zone.TypeTXT, `"Keep.This.Case."`, `"Keep.This.Case."`},
		{"txt gains quotes", zone.TypeTXT, "hello", `"hello"`},
		{"txt with several strings", zone.TypeTXT, `"a" "b"`, `"a" "b"`},

		{"soa across lines", zone.TypeSOA, "ns.example.com. hostmaster.example.com. (\n1 2 3 4 5 )",
			"ns.example.com. hostmaster.example.com. 1 2 3 4 5"},
		{"caa", zone.TypeCAA, `0 issue "letsencrypt.org"`, `0 issue "letsencrypt.org"`},

		// RFC 3597 §5.
		{"unknown type in hex form", zone.RRType(65280), `\# 4 0A0B0C0D`, `\# 4 0A0B0C0D`},
		{"unknown type of zero length", zone.RRType(65280), `\# 0`, `\# 0`},
		{"known type given in hex form is converted", zone.TypeA, `\# 4 C0000201`, "192.0.2.1"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := zone.ParseRData(tc.typ, zone.ClassIN, tc.in)
			if err != nil {
				t.Fatalf("ParseRData(%s, %q): %v", tc.typ, tc.in, err)
			}
			if got.String() != tc.want {
				t.Errorf("ParseRData(%s, %q) = %q, want %q", tc.typ, tc.in, got.String(), tc.want)
			}

			// Canonicalisation must be a fixed point, or the stored value
			// would differ from what a re-import produces.
			again, err := zone.ParseRData(tc.typ, zone.ClassIN, got.String())
			if err != nil {
				t.Fatalf("re-parsing the canonical form %q failed: %v", got.String(), err)
			}
			if !again.Equal(got) {
				t.Errorf("canonicalisation is not idempotent: %q then %q", got.String(), again.String())
			}
		})
	}
}

// TestParseRDataEscapeConvergence is the property ADR 0001 rests on. Two
// spellings of the same name must produce byte-identical data, otherwise the
// unique index on (zone, name, class, type, data) fails to catch a duplicate
// and an RRset ends up holding the same record twice.
func TestParseRDataEscapeConvergence(t *testing.T) {
	t.Parallel()

	groups := [][]string{
		// A literal letter, the same letter in the other case, and its decimal
		// escape all denote one octet (RFC 1035 §5.1, RFC 4343).
		{"a.example.com.", "A.example.com.", `\065.example.com.`, `\097.example.com.`},
		// An escaped character that needs no escaping.
		{"foo.example.com.", `f\oo.example.com.`, `\102oo.example.com.`},
		// A dot inside a label, written two ways.
		{`a\.b.example.com.`, `a\046b.example.com.`},
	}

	for _, group := range groups {
		t.Run(group[0], func(t *testing.T) {
			t.Parallel()

			want, err := zone.ParseRData(zone.TypeCNAME, zone.ClassIN, group[0])
			if err != nil {
				t.Fatalf("ParseRData(%q): %v", group[0], err)
			}
			for _, spelling := range group[1:] {
				got, err := zone.ParseRData(zone.TypeCNAME, zone.ClassIN, spelling)
				if err != nil {
					t.Fatalf("ParseRData(%q): %v", spelling, err)
				}
				if !got.Equal(want) {
					t.Errorf("%q canonicalised to %q, but %q gave %q",
						spelling, got.String(), group[0], want.String())
				}
			}
		})
	}
}

func TestParseRDataRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		typ   zone.RRType
		class zone.Class
		in    string
	}{
		{"empty", zone.TypeA, zone.ClassIN, ""},
		{"only space", zone.TypeA, zone.ClassIN, "   "},
		{"not an address", zone.TypeA, zone.ClassIN, "not-an-ip"},
		{"address with leading zeroes", zone.TypeA, zone.ClassIN, "192.000.002.001"},
		{"ipv6 in an a record", zone.TypeA, zone.ClassIN, "2001:db8::1"},
		{"ipv4 in an aaaa record", zone.TypeAAAA, zone.ClassIN, "192.0.2.1"},
		{"mx without a preference", zone.TypeMX, zone.ClassIN, "mail.example.com."},
		{"trailing junk", zone.TypeA, zone.ClassIN, "192.0.2.1 192.0.2.2"},
		{"hex length disagrees with the data", zone.RRType(65280), zone.ClassIN, `\# 4 0A0B`},

		// A relative name would otherwise be silently attached to whatever
		// origin happened to be in scope.
		{"relative cname target", zone.TypeCNAME, zone.ClassIN, "foo"},
		{"relative mx target", zone.TypeMX, zone.ClassIN, "10 mail"},
		{"partially qualified", zone.TypeCNAME, zone.ClassIN, "foo.example.com"},

		// A smuggled newline must not turn one field into two records.
		{"second record smuggled in", zone.TypeA, zone.ClassIN, "192.0.2.1\nevil. 0 IN A 192.0.2.2"},

		// Types and classes that exist only inside a message.
		{"opt is a meta type", zone.TypeOPT, zone.ClassIN, `\# 0`},
		{"any is a query type", zone.TypeANY, zone.ClassIN, "192.0.2.1"},
		{"null is not allowed in a zone", zone.TypeNULL, zone.ClassIN, `\# 0`},
		{"class any", zone.TypeA, zone.ClassANY, "192.0.2.1"},
		{"class none", zone.TypeA, zone.ClassNONE, "192.0.2.1"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := zone.ParseRData(tc.typ, tc.class, tc.in)
			if !errors.Is(err, zone.ErrInvalidRData) {
				t.Fatalf("ParseRData(%s, %s, %q) = %q, %v; want ErrInvalidRData",
					tc.typ, tc.class, tc.in, got.String(), err)
			}
			if !errors.Is(err, zone.ErrInvalid) {
				t.Errorf("error %v does not wrap ErrInvalid", err)
			}
			if strings.Contains(err.Error(), "at line:") {
				t.Errorf("error leaks the synthetic parse position: %v", err)
			}
			if strings.Contains(err.Error(), "probe") {
				t.Errorf("error leaks the probe origin: %v", err)
			}
		})
	}
}

func TestParseRDataOtherClasses(t *testing.T) {
	t.Parallel()

	// version.bind lives in the CHAOS class, the one place a second class
	// shows up in practice.
	got, err := zone.ParseRData(zone.TypeTXT, zone.ClassCH, `"9.9.9"`)
	if err != nil {
		t.Fatalf("ParseRData: %v", err)
	}
	if got.String() != `"9.9.9"` {
		t.Errorf("got %q", got.String())
	}

	// An unassigned class uses the CLASS<n> form of RFC 3597 §5.
	if _, err := zone.ParseRData(zone.RRType(65280), zone.Class(42), `\# 2 ABCD`); err != nil {
		t.Errorf("unassigned class: %v", err)
	}
}

func TestRDataAddress(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		typ  zone.RRType
		in   string
		want string // empty means no address
	}{
		{"a", zone.TypeA, "192.0.2.1", "192.0.2.1"},
		{"aaaa", zone.TypeAAAA, "2001:db8::1", "2001:db8::1"},
		{"cname has no address", zone.TypeCNAME, "foo.example.com.", ""},
		{"txt has no address", zone.TypeTXT, `"192.0.2.1"`, ""},
		{"mx has no address", zone.TypeMX, "10 mail.example.com.", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rd, err := zone.ParseRData(tc.typ, zone.ClassIN, tc.in)
			if err != nil {
				t.Fatalf("ParseRData: %v", err)
			}
			got, ok := rd.Address(tc.typ)
			if tc.want == "" {
				if ok {
					t.Fatalf("Address() = %v, want none", got)
				}
				return
			}
			if !ok {
				t.Fatalf("Address() reported none, want %s", tc.want)
			}
			if got != netip.MustParseAddr(tc.want) {
				t.Errorf("Address() = %v, want %s", got, tc.want)
			}
			// The store writes four octets for A and sixteen for AAAA, so the
			// family has to match the record type exactly.
			if (tc.typ == zone.TypeA) != got.Is4() {
				t.Errorf("%s produced an address of the wrong family: %v", tc.typ, got)
			}
		})
	}
}

func TestRDataZeroValue(t *testing.T) {
	t.Parallel()

	var rd zone.RData
	if !rd.IsZero() {
		t.Error("the zero value should report IsZero")
	}
	if rd.String() != "" {
		t.Errorf("zero String() = %q, want empty", rd.String())
	}
	if _, ok := rd.Address(zone.TypeA); ok {
		t.Error("the zero value should carry no address")
	}
}

// TestParseRDataEmptyPerType guards the case the wire library is happy to
// accept: data that is entirely absent. It zero-fills the record instead of
// failing, producing an A record with no address or a CAA record with an empty
// tag and value: plausible-looking rows that are simply wrong.
func TestParseRDataEmptyPerType(t *testing.T) {
	t.Parallel()

	types := []zone.RRType{
		zone.TypeA, zone.TypeAAAA, zone.TypeNS, zone.TypeCNAME, zone.TypePTR,
		zone.TypeSOA, zone.TypeMX, zone.TypeTXT, zone.TypeSRV, zone.TypeCAA,
		zone.TypeNAPTR, zone.TypeTLSA, zone.TypeSSHFP, zone.TypeHTTPS,
		zone.TypeSVCB, zone.TypeDNAME, zone.TypeHINFO, zone.RRType(65280),
	}

	for _, typ := range types {
		t.Run(typ.String(), func(t *testing.T) {
			t.Parallel()

			for _, in := range []string{"", " ", "\t", "\n"} {
				got, err := zone.ParseRData(typ, zone.ClassIN, in)
				if !errors.Is(err, zone.ErrInvalidRData) {
					t.Errorf("ParseRData(%s, %q) = %q, %v; want ErrInvalidRData",
						typ, in, got.String(), err)
				}
			}
		})
	}
}

// TestParseRDataEscapedNamesSurviveRoundTrip guards the seam between our own
// name escaping and the wire library's parser. ParseRData refuses data it
// cannot read back, so a character we escape but the library then renders
// differently would reject a perfectly legal name.
func TestParseRDataEscapedNamesSurviveRoundTrip(t *testing.T) {
	t.Parallel()

	// Every octet that Name.String escapes, plus the boundaries.
	names := []string{
		`a\.b.example.com.`,
		`a\\b.example.com.`,
		`a\"b.example.com.`,
		`a\;b.example.com.`,
		`a\(b.example.com.`,
		`a\)b.example.com.`,
		`a\@b.example.com.`,
		`a\$b.example.com.`,
		`a\'b.example.com.`,
		`a\032b.example.com.`,
		`\000.example.com.`,
		`\255.example.com.`,
		`\001\002\003.example.com.`,
		"*.example.com.",
		"_dmarc.example.com.",
		".",
	}

	for _, n := range names {
		t.Run(n, func(t *testing.T) {
			t.Parallel()

			got, err := zone.ParseRData(zone.TypeCNAME, zone.ClassIN, n)
			if err != nil {
				t.Fatalf("ParseRData(CNAME, %q): %v", n, err)
			}
			// The data is one name, so it must equal that name's canonical form.
			want := zone.MustParseName(n).String()
			if got.String() != want {
				t.Errorf("ParseRData(CNAME, %q) = %q, want %q", n, got.String(), want)
			}
		})
	}
}

// TestParseRDataRejectsUnstableForms covers data the wire library accepts but
// prints in a form it will not read back. Found by FuzzParseRData.
func TestParseRDataRejectsUnstableForms(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		typ  zone.RRType
		in   string
	}{
		// A SIG covering type 0 prints its covered type as the placeholder
		// "None", which the parser then rejects.
		{"sig covering type zero", zone.TypeSIG, "()"},
		// Empty parentheses are not whitespace, so they slip past the check
		// for absent data and reach the parser as nothing at all.
		{"empty parentheses", zone.TypeA, "()"},
		{"empty parentheses across lines", zone.TypeA, "(\n)"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := zone.ParseRData(tc.typ, zone.ClassIN, tc.in)
			if !errors.Is(err, zone.ErrInvalidRData) {
				t.Fatalf("ParseRData(%s, %q) = %q, %v; want ErrInvalidRData",
					tc.typ, tc.in, got.String(), err)
			}
		})
	}
}

// TestEscapingMatchesWireLibrary pins our name escaping to the wire library's.
//
// Owner names are printed by Name.String, while names inside record data are
// printed by the wire library. If the two conventions differed for even one
// octet, the same name would be stored with two spellings depending on where it
// appeared, and the unique index over an RRset would stop catching duplicates.
// A library upgrade that changed the convention must fail here rather than in
// production.
func TestEscapingMatchesWireLibrary(t *testing.T) {
	t.Parallel()

	for b := range 256 {
		octet := byte(b)
		// A label built from the octet, written numerically so the input form
		// is never itself ambiguous.
		in := fmt.Sprintf(`x\%03dy.example.com.`, b)

		ours := zone.MustParseName(in).String()

		// Round-tripping through record data goes through the library's
		// printer, so this is the form that would be stored for a CNAME.
		theirs, err := zone.ParseRData(zone.TypeCNAME, zone.ClassIN, in)
		if err != nil {
			t.Fatalf("octet %d (%q): ParseRData: %v", b, string(octet), err)
		}

		if ours != theirs.String() {
			t.Errorf("octet %d (%q): owner name prints as %q but record data as %q",
				b, string(octet), ours, theirs.String())
		}
	}
}

// The store reads record data back on every snapshot rebuild, so what a single
// reconstruction costs decides whether it may go through the full parser.
func BenchmarkParseRData(b *testing.B) {
	cases := []struct {
		name  string
		typ   zone.RRType
		rdata string
	}{
		{"A", zone.TypeA, "192.0.2.10"},
		{"AAAA", zone.TypeAAAA, "2001:db8::1"},
		{"MX", zone.TypeMX, "10 mail.example.com."},
		{"TXT", zone.TypeTXT, `"v=spf1 include:_spf.example.com ~all"`},
	}

	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			for b.Loop() {
				if _, err := zone.ParseRData(c.typ, zone.ClassIN, c.rdata); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkParseName(b *testing.B) {
	for b.Loop() {
		if _, err := zone.ParseName("www.example.com."); err != nil {
			b.Fatal(err)
		}
	}
}
