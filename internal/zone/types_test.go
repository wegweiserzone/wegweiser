package zone_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/miekg/dns"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

func TestRRTypeString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   zone.RRType
		want string
	}{
		{"a", zone.TypeA, "A"},
		{"aaaa", zone.TypeAAAA, "AAAA"},
		{"soa", zone.TypeSOA, "SOA"},
		{"https", zone.TypeHTTPS, "HTTPS"},
		{"any is a real mnemonic", zone.TypeANY, "ANY"},
		// RFC 3597 §5: a type with no mnemonic is written as TYPE<number>.
		{"unassigned", zone.RRType(65280), "TYPE65280"},
		// The wire library maps these to the placeholders "None" and
		// "Reserved", which are not usable mnemonics and must not leak out.
		{"type zero", zone.TypeNone, "TYPE0"},
		{"type 65535", zone.RRType(65535), "TYPE65535"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.in.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

// HasMnemonic is what anything grouping queries by type folds on, so what it
// says has to agree with what String prints: a type String writes in the
// TYPE<number> form of RFC 3597 §5 is one without a mnemonic.
func TestRRTypeHasMnemonic(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		in   zone.RRType
		want bool
	}{
		{"a", zone.TypeA, true},
		{"any", zone.TypeANY, true},
		{"opt", zone.TypeOPT, true},
		{"unassigned", zone.RRType(65280), false},
		// "None" and "Reserved" are the wire library's placeholders, not
		// mnemonics anybody may write.
		{"type zero", zone.TypeNone, false},
		{"type 65535", zone.RRType(65535), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.in.HasMnemonic(); got != tc.want {
				t.Errorf("HasMnemonic() = %v, want %v", got, tc.want)
			}
			if printed := strings.HasPrefix(tc.in.String(), "TYPE"); printed == tc.want {
				t.Errorf("HasMnemonic() = %v but String() = %q", tc.want, tc.in.String())
			}
		})
	}
}

func TestParseRRType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    zone.RRType
		wantErr bool
	}{
		{"mnemonic", "A", zone.TypeA, false},
		{"lowercase", "aaaa", zone.TypeAAAA, false},
		{"mixed case", "CnAmE", zone.TypeCNAME, false},
		{"surrounding space", "  MX  ", zone.TypeMX, false},
		{"rfc3597 form of a known type", "TYPE1", zone.TypeA, false},
		{"rfc3597 form of an unknown type", "TYPE65280", zone.RRType(65280), false},
		{"query type parses; storability is a separate question", "AXFR", zone.TypeAXFR, false},
		{"empty", "", 0, true},
		{"nonsense", "NOTATYPE", 0, true},
		{"placeholder None is not accepted", "NONE", 0, true},
		{"placeholder Reserved is not accepted", "RESERVED", 0, true},
		{"type number out of range", "TYPE65536", 0, true},
		{"type prefix without a number", "TYPE", 0, true},
		{"negative type number", "TYPE-1", 0, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := zone.ParseRRType(tc.in)
			if tc.wantErr {
				if !errors.Is(err, zone.ErrInvalidRRType) {
					t.Fatalf("ParseRRType(%q) error = %v, want ErrInvalidRRType", tc.in, err)
				}
				if !errors.Is(err, zone.ErrInvalid) {
					t.Errorf("error %v does not wrap ErrInvalid", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRRType(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseRRType(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestRRTypeRoundTrip checks the property the storage layer depends on: every
// type number prints to something that parses back to the same number.
func TestRRTypeRoundTrip(t *testing.T) {
	t.Parallel()

	for i := range 1 << 16 {
		rt := zone.RRType(i)
		back, err := zone.ParseRRType(rt.String())
		if err != nil {
			t.Fatalf("type %d printed %q, which does not parse: %v", i, rt.String(), err)
		}
		if back != rt {
			t.Fatalf("type %d printed %q and parsed back as %d", i, rt.String(), back)
		}
	}
}

func TestRRTypeStorable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   zone.RRType
		want bool
	}{
		{"a", zone.TypeA, true},
		{"soa", zone.TypeSOA, true},
		{"unassigned types are storable, per RFC 3597", zone.RRType(65280), true},
		{"type zero is reserved", zone.TypeNone, false},
		// RFC 1035 §3.3.10 says NULL is not allowed in zone files.
		{"null", zone.TypeNULL, false},
		{"opt is a meta type", zone.TypeOPT, false},
		{"tsig is a meta type", zone.TypeTSIG, false},
		{"tkey is a meta type", zone.TypeTKEY, false},
		{"axfr is a query type", zone.TypeAXFR, false},
		{"ixfr is a query type", zone.TypeIXFR, false},
		{"any is a query type", zone.TypeANY, false},
		{"maila is a query type", zone.TypeMAILA, false},
		{"mailb is a query type", zone.TypeMAILB, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.in.Storable(); got != tc.want {
				t.Errorf("%s.Storable() = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestRRTypeConstantsMatchIANA(t *testing.T) {
	t.Parallel()

	// The constants are defined in terms of the wire library's, so this guards
	// against a mistyped name rather than against a changed number.
	pairs := map[zone.RRType]uint16{
		zone.TypeA:     dns.TypeA,
		zone.TypeNS:    dns.TypeNS,
		zone.TypeCNAME: dns.TypeCNAME,
		zone.TypeSOA:   dns.TypeSOA,
		zone.TypePTR:   dns.TypePTR,
		zone.TypeMX:    dns.TypeMX,
		zone.TypeTXT:   dns.TypeTXT,
		zone.TypeAAAA:  dns.TypeAAAA,
		zone.TypeSRV:   dns.TypeSRV,
		zone.TypeDNAME: dns.TypeDNAME,
		zone.TypeOPT:   dns.TypeOPT,
		zone.TypeANY:   dns.TypeANY,
	}
	for got, want := range pairs {
		if uint16(got) != want {
			t.Errorf("%s = %d, want %d", got, got, want)
		}
	}
}

func TestClass(t *testing.T) {
	t.Parallel()

	t.Run("string", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			in   zone.Class
			want string
		}{
			{zone.ClassIN, "IN"},
			{zone.ClassCH, "CH"},
			{zone.ClassHS, "HS"},
			{zone.ClassNONE, "NONE"},
			{zone.ClassANY, "ANY"},
			{zone.Class(0), "CLASS0"},
			{zone.Class(42), "CLASS42"},
		}
		for _, tc := range tests {
			if got := tc.in.String(); got != tc.want {
				t.Errorf("Class(%d).String() = %q, want %q", uint16(tc.in), got, tc.want)
			}
		}
	})

	t.Run("parse", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			in      string
			want    zone.Class
			wantErr bool
		}{
			{"IN", zone.ClassIN, false},
			{"in", zone.ClassIN, false},
			{"CLASS1", zone.ClassIN, false},
			{"CLASS42", zone.Class(42), false},
			{"", 0, true},
			{"NOPE", 0, true},
			{"CLASS65536", 0, true},
		}
		for _, tc := range tests {
			got, err := zone.ParseClass(tc.in)
			if tc.wantErr {
				if !errors.Is(err, zone.ErrInvalidClass) {
					t.Errorf("ParseClass(%q) error = %v, want ErrInvalidClass", tc.in, err)
				}
				continue
			}
			if err != nil {
				t.Errorf("ParseClass(%q): %v", tc.in, err)
				continue
			}
			if got != tc.want {
				t.Errorf("ParseClass(%q) = %d, want %d", tc.in, got, tc.want)
			}
		}
	})

	t.Run("round trip over the whole range", func(t *testing.T) {
		t.Parallel()

		for i := range 1 << 16 {
			c := zone.Class(i)
			back, err := zone.ParseClass(c.String())
			if err != nil {
				t.Fatalf("class %d printed %q, which does not parse: %v", i, c.String(), err)
			}
			if back != c {
				t.Fatalf("class %d printed %q and parsed back as %d", i, c.String(), back)
			}
		}
	})

	t.Run("storable", func(t *testing.T) {
		t.Parallel()

		// NONE and ANY are QCLASSes and belong in a message, not in a zone.
		for c, want := range map[zone.Class]bool{
			zone.ClassIN:   true,
			zone.ClassCH:   true,
			zone.ClassHS:   true,
			zone.Class(0):  false,
			zone.ClassNONE: false,
			zone.ClassANY:  false,
		} {
			if got := c.Storable(); got != want {
				t.Errorf("%s.Storable() = %v, want %v", c, got, want)
			}
		}
	})
}

func TestTypeAndClassJSON(t *testing.T) {
	t.Parallel()

	type rr struct {
		Type  zone.RRType `json:"type"`
		Class zone.Class  `json:"class"`
	}

	in := rr{Type: zone.TypeAAAA, Class: zone.ClassIN}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if want := `{"type":"AAAA","class":"IN"}`; string(b) != want {
		t.Fatalf("Marshal = %s, want %s", b, want)
	}

	var out rr
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("round trip = %+v, want %+v", out, in)
	}

	if err := json.Unmarshal([]byte(`{"type":"NOPE"}`), &out); !errors.Is(err, zone.ErrInvalidRRType) {
		t.Errorf("decoding an unknown type: error = %v, want ErrInvalidRRType", err)
	}
}
