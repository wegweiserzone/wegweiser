package zone_test

import (
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// rec builds a record for a test, failing the test if the input is not valid.
func rec(t *testing.T, name string, ttl zone.TTL, typ zone.RRType, rdata string) zone.Record {
	t.Helper()

	r, err := zone.NewRecord("z", zone.MustParseName(name), zone.ClassIN, typ, ttl, rdata)
	if err != nil {
		t.Fatalf("NewRecord(%q %s %q): %v", name, typ, rdata, err)
	}
	return r
}

func TestNewRecordRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		typ   zone.RRType
		rdata string
		want  string // a phrase the error must contain
	}{
		// The SOA lives on the zone, not among its records: its serial belongs
		// to the journal, one commit per step.
		{"soa as a record", zone.TypeSOA, "ns.example.com. hm.example.com. 1 2 3 4 5", "start-of-authority"},
		{"opt", zone.TypeOPT, `\# 0`, "only inside a message"},
		{"any", zone.TypeANY, "192.0.2.1", "only inside a message"},
		// The header is checked before the data, so this reports what an RRSIG
		// is rather than what its data happened to be missing.
		{"rrsig", zone.TypeRRSIG, `\# 0`, "does not sign zones yet"},
		{"bad data", zone.TypeA, "not-an-ip", "record data"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := zone.NewRecord("z", zone.MustParseName("x.example.com."),
				zone.ClassIN, tc.typ, 3600, tc.rdata)
			if !errors.Is(err, zone.ErrInvalid) {
				t.Fatalf("error = %v, want ErrInvalid", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should mention %q", err, tc.want)
			}
		})
	}

	t.Run("dnssec types explain why", func(t *testing.T) {
		t.Parallel()

		// DNSKEY parses, so this must be refused by the record rules rather
		// than by the data parser.
		_, err := zone.NewRecord("z", zone.MustParseName("example.com."), zone.ClassIN,
			zone.TypeDNSKEY, 3600, "256 3 8 AwEAAaz/tAm8yTn4Mfeh5eyI96WSVexTBAvkMgJzkKTOiW1vkIbzxeF3")
		if !errors.Is(err, zone.ErrInvalid) {
			t.Fatalf("error = %v, want ErrInvalid", err)
		}
		if !strings.Contains(err.Error(), "sign") {
			t.Errorf("error should say why: %v", err)
		}
	})
}

func TestValidateRRset(t *testing.T) {
	t.Parallel()

	t.Run("empty is valid", func(t *testing.T) {
		t.Parallel()

		if err := zone.ValidateRRset(nil); err != nil {
			t.Errorf("an empty RRset is what deleting the last member leaves: %v", err)
		}
	})

	t.Run("several addresses share one TTL", func(t *testing.T) {
		t.Parallel()

		set := []zone.Record{
			rec(t, "www.example.com.", 3600, zone.TypeA, "192.0.2.1"),
			rec(t, "www.example.com.", 3600, zone.TypeA, "192.0.2.2"),
		}
		if err := zone.ValidateRRset(set); err != nil {
			t.Errorf("ValidateRRset: %v", err)
		}
	})

	t.Run("divergent TTLs are refused", func(t *testing.T) {
		t.Parallel()

		set := []zone.Record{
			rec(t, "www.example.com.", 3600, zone.TypeA, "192.0.2.1"),
			rec(t, "www.example.com.", 300, zone.TypeA, "192.0.2.2"),
		}
		err := zone.ValidateRRset(set)
		if !errors.Is(err, zone.ErrInvalid) {
			t.Fatalf("error = %v, want ErrInvalid", err)
		}
		if !strings.Contains(err.Error(), "2181") {
			t.Errorf("the error should cite the rule: %v", err)
		}
	})

	t.Run("duplicate data is refused", func(t *testing.T) {
		t.Parallel()

		set := []zone.Record{
			rec(t, "www.example.com.", 3600, zone.TypeA, "192.0.2.1"),
			rec(t, "www.example.com.", 3600, zone.TypeA, "192.0.2.1"),
		}
		if err := zone.ValidateRRset(set); !errors.Is(err, zone.ErrInvalid) {
			t.Errorf("error = %v, want ErrInvalid", err)
		}
	})

	t.Run("duplicate data written differently is still a duplicate", func(t *testing.T) {
		t.Parallel()

		// This is what canonical data buys: two spellings of one target are
		// caught as the duplicate they are.
		set := []zone.Record{
			rec(t, "www.example.com.", 3600, zone.TypeCNAME, "target.example.com."),
			rec(t, "www.example.com.", 3600, zone.TypeCNAME, "TARGET.Example.COM."),
		}
		if err := zone.ValidateRRset(set); !errors.Is(err, zone.ErrInvalid) {
			t.Errorf("error = %v, want ErrInvalid", err)
		}
	})

	t.Run("two CNAMEs at one name are refused", func(t *testing.T) {
		t.Parallel()

		set := []zone.Record{
			rec(t, "www.example.com.", 3600, zone.TypeCNAME, "a.example.com."),
			rec(t, "www.example.com.", 3600, zone.TypeCNAME, "b.example.com."),
		}
		err := zone.ValidateRRset(set)
		if !errors.Is(err, zone.ErrInvalid) {
			t.Fatalf("error = %v, want ErrInvalid", err)
		}
		if !strings.Contains(err.Error(), "canonical name") {
			t.Errorf("the error should explain: %v", err)
		}
	})

	t.Run("mixed types are not one RRset", func(t *testing.T) {
		t.Parallel()

		set := []zone.Record{
			rec(t, "www.example.com.", 3600, zone.TypeA, "192.0.2.1"),
			rec(t, "www.example.com.", 3600, zone.TypeAAAA, "2001:db8::1"),
		}
		if err := zone.ValidateRRset(set); !errors.Is(err, zone.ErrInvalid) {
			t.Errorf("error = %v, want ErrInvalid", err)
		}
	})
}

func TestValidateOwner(t *testing.T) {
	t.Parallel()

	z := newTestZone(t, "example.com.")

	t.Run("a CNAME cannot share a name with other data", func(t *testing.T) {
		t.Parallel()

		name := zone.MustParseName("www.example.com.")
		records := []zone.Record{
			rec(t, "www.example.com.", 3600, zone.TypeCNAME, "target.example.com."),
			rec(t, "www.example.com.", 3600, zone.TypeA, "192.0.2.1"),
		}
		err := zone.ValidateOwner(z, name, records)
		if !errors.Is(err, zone.ErrInvalid) {
			t.Fatalf("error = %v, want ErrInvalid", err)
		}
		if !strings.Contains(err.Error(), "10.1") {
			t.Errorf("the error should cite RFC 2181 §10.1: %v", err)
		}
	})

	t.Run("a CNAME alone is fine", func(t *testing.T) {
		t.Parallel()

		name := zone.MustParseName("www.example.com.")
		records := []zone.Record{rec(t, "www.example.com.", 3600, zone.TypeCNAME, "target.example.com.")}
		if err := zone.ValidateOwner(z, name, records); err != nil {
			t.Errorf("ValidateOwner: %v", err)
		}
	})

	t.Run("the apex cannot be a CNAME", func(t *testing.T) {
		t.Parallel()

		name := zone.MustParseName("example.com.")
		records := []zone.Record{rec(t, "example.com.", 3600, zone.TypeCNAME, "elsewhere.example.net.")}
		err := zone.ValidateOwner(z, name, records)
		if !errors.Is(err, zone.ErrInvalid) {
			t.Fatalf("error = %v, want ErrInvalid", err)
		}
		if !strings.Contains(err.Error(), "apex") {
			t.Errorf("the error should say it is the apex: %v", err)
		}
	})

	t.Run("a name outside the zone is refused", func(t *testing.T) {
		t.Parallel()

		name := zone.MustParseName("www.example.net.")
		if err := zone.ValidateOwner(z, name, nil); !errors.Is(err, zone.ErrInvalid) {
			t.Errorf("error = %v, want ErrInvalid", err)
		}
	})

	t.Run("a PTR outside the zone's network is refused", func(t *testing.T) {
		t.Parallel()

		rev := newTestZone(t, "2.0.192.in-addr.arpa.")
		// 192.0.3.10 is not inside 192.0.2.0/24, so a resolver asking for it
		// would never reach this zone.
		name := zone.MustParseName("10.3.0.192.in-addr.arpa.")
		records := []zone.Record{rec(t, "10.3.0.192.in-addr.arpa.", 3600, zone.TypePTR, "www.example.com.")}

		// The name is outside the zone entirely, which is caught first.
		if err := zone.ValidateOwner(rev, name, records); !errors.Is(err, zone.ErrInvalid) {
			t.Errorf("error = %v, want ErrInvalid", err)
		}
	})

	t.Run("a PTR inside the network is fine", func(t *testing.T) {
		t.Parallel()

		rev := newTestZone(t, "2.0.192.in-addr.arpa.")
		name := zone.MustParseName("10.2.0.192.in-addr.arpa.")
		records := []zone.Record{rec(t, "10.2.0.192.in-addr.arpa.", 3600, zone.TypePTR, "www.example.com.")}
		if err := zone.ValidateOwner(rev, name, records); err != nil {
			t.Errorf("ValidateOwner: %v", err)
		}
	})
}

// apexNS is the minimum a zone needs to be a zone.
func apexNS(t *testing.T) []zone.Record {
	t.Helper()
	return []zone.Record{rec(t, "example.com.", 3600, zone.TypeNS, "ns1.example.com.")}
}

func TestValidateZone(t *testing.T) {
	t.Parallel()

	z := newTestZone(t, "example.com.")

	t.Run("a minimal zone", func(t *testing.T) {
		t.Parallel()

		if err := zone.ValidateZone(z, apexNS(t)); err != nil {
			t.Errorf("ValidateZone: %v", err)
		}
	})

	t.Run("a zone with no apex NS is refused", func(t *testing.T) {
		t.Parallel()

		records := []zone.Record{rec(t, "www.example.com.", 3600, zone.TypeA, "192.0.2.1")}
		err := zone.ValidateZone(z, records)
		if !errors.Is(err, zone.ErrInvalid) {
			t.Fatalf("error = %v, want ErrInvalid", err)
		}
		if !strings.Contains(err.Error(), "NS record at its apex") {
			t.Errorf("the error should name the problem: %v", err)
		}
	})

	t.Run("a record outside the zone is refused", func(t *testing.T) {
		t.Parallel()

		records := append(apexNS(t), rec(t, "www.example.net.", 3600, zone.TypeA, "192.0.2.1"))
		if err := zone.ValidateZone(z, records); !errors.Is(err, zone.ErrInvalid) {
			t.Errorf("error = %v, want ErrInvalid", err)
		}
	})
}

// TestValidateZoneDelegations covers RFC 1034 §4.2.1: at a delegation the
// parent keeps only NS records, and below it only glue survives.
func TestValidateZoneDelegations(t *testing.T) {
	t.Parallel()

	z := newTestZone(t, "example.com.")

	t.Run("a delegation with glue is fine", func(t *testing.T) {
		t.Parallel()

		records := append(apexNS(t),
			rec(t, "sub.example.com.", 3600, zone.TypeNS, "ns1.sub.example.com."),
			rec(t, "ns1.sub.example.com.", 3600, zone.TypeA, "192.0.2.53"),
			rec(t, "ns1.sub.example.com.", 3600, zone.TypeAAAA, "2001:db8::53"),
		)
		if err := zone.ValidateZone(z, records); err != nil {
			t.Errorf("ValidateZone: %v", err)
		}
	})

	t.Run("other data at the delegation point is refused", func(t *testing.T) {
		t.Parallel()

		records := append(apexNS(t),
			rec(t, "sub.example.com.", 3600, zone.TypeNS, "ns1.sub.example.com."),
			// Never answered: a query for sub.example.com is referred away.
			rec(t, "sub.example.com.", 3600, zone.TypeA, "192.0.2.1"),
		)
		err := zone.ValidateZone(z, records)
		if !errors.Is(err, zone.ErrInvalid) {
			t.Fatalf("error = %v, want ErrInvalid", err)
		}
		if !strings.Contains(err.Error(), "referred") {
			t.Errorf("the error should explain why it is invisible: %v", err)
		}
	})

	t.Run("non-glue below a delegation is refused", func(t *testing.T) {
		t.Parallel()

		records := append(apexNS(t),
			rec(t, "sub.example.com.", 3600, zone.TypeNS, "ns1.sub.example.com."),
			rec(t, "www.sub.example.com.", 3600, zone.TypeTXT, `"occluded"`),
		)
		err := zone.ValidateZone(z, records)
		if !errors.Is(err, zone.ErrInvalid) {
			t.Fatalf("error = %v, want ErrInvalid", err)
		}
		if !strings.Contains(err.Error(), "glue") {
			t.Errorf("the error should mention glue: %v", err)
		}
	})

	t.Run("the apex NS set is not a delegation", func(t *testing.T) {
		t.Parallel()

		// The zone's own NS records sit at the apex alongside everything else,
		// and must not make the whole zone look delegated away.
		records := append(apexNS(t),
			rec(t, "example.com.", 3600, zone.TypeMX, "10 mail.example.com."),
			rec(t, "www.example.com.", 3600, zone.TypeA, "192.0.2.1"),
		)
		if err := zone.ValidateZone(z, records); err != nil {
			t.Errorf("ValidateZone: %v", err)
		}
	})
}

func TestRecordHelpers(t *testing.T) {
	t.Parallel()

	t.Run("address", func(t *testing.T) {
		t.Parallel()

		a := rec(t, "www.example.com.", 3600, zone.TypeA, "192.0.2.1")
		got, ok := a.Address()
		if !ok || got != netip.MustParseAddr("192.0.2.1") {
			t.Errorf("Address() = %v, %v", got, ok)
		}

		txt := rec(t, "www.example.com.", 3600, zone.TypeTXT, `"192.0.2.1"`)
		if _, ok := txt.Address(); ok {
			t.Error("a TXT record carries no address")
		}
	})

	t.Run("string is one zonefile line", func(t *testing.T) {
		t.Parallel()

		r := rec(t, "www.example.com.", 3600, zone.TypeMX, "10 Mail.Example.COM.")
		want := "www.example.com.\t3600\tIN\tMX\t10 mail.example.com."
		if got := r.String(); got != want {
			t.Errorf("String() = %q, want %q", got, want)
		}
	})

	t.Run("compare orders by name then type then data", func(t *testing.T) {
		t.Parallel()

		ordered := []zone.Record{
			rec(t, "example.com.", 3600, zone.TypeNS, "ns1.example.com."),
			rec(t, "a.example.com.", 3600, zone.TypeA, "192.0.2.1"),
			rec(t, "a.example.com.", 3600, zone.TypeA, "192.0.2.2"),
			rec(t, "a.example.com.", 3600, zone.TypeAAAA, "2001:db8::1"),
			rec(t, "b.example.com.", 3600, zone.TypeA, "192.0.2.1"),
		}
		for i := range ordered {
			for j := range ordered {
				got := sign(ordered[i].Compare(ordered[j]))
				var want int
				switch {
				case i < j:
					want = -1
				case i > j:
					want = 1
				}
				if got != want {
					t.Errorf("Compare(%d, %d) = %d, want %d\n  %s\n  %s",
						i, j, got, want, ordered[i], ordered[j])
				}
			}
		}
	})

	t.Run("managed provenance needs both halves", func(t *testing.T) {
		t.Parallel()

		r := rec(t, "10.2.0.192.in-addr.arpa.", 3600, zone.TypePTR, "www.example.com.")
		if r.IsManaged() {
			t.Error("an authored record is not managed")
		}

		r.ManagedBy = "source"
		if err := r.Validate(); !errors.Is(err, zone.ErrInvalid) {
			t.Errorf("a source without a reason should be refused: %v", err)
		}

		r.ManagedKind = zone.ManagedPTR
		if err := r.Validate(); err != nil {
			t.Errorf("Validate: %v", err)
		}
		if !r.IsManaged() {
			t.Error("IsManaged should report true once provenance is set")
		}
	})
}
