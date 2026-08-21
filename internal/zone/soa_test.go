package zone_test

import (
	"errors"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

func testSOA() zone.SOA {
	return zone.DefaultSOA(
		zone.MustParseName("ns1.example.com."),
		zone.MustParseName("hostmaster.example.com."),
	)
}

func TestDefaultSOAIsValid(t *testing.T) {
	t.Parallel()

	if err := testSOA().Validate(); err != nil {
		t.Fatalf("the defaults we hand a new zone must validate: %v", err)
	}
}

func TestSOAValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*zone.SOA)
		wantErr bool
	}{
		{"defaults", func(*zone.SOA) {}, false},
		{"no primary", func(s *zone.SOA) { s.NS = zone.Name{} }, true},
		{"root as primary", func(s *zone.SOA) { s.NS = zone.Root }, true},
		{"no mailbox", func(s *zone.SOA) { s.Mbox = zone.Name{} }, true},

		{"refresh of zero", func(s *zone.SOA) { s.Refresh = 0 }, true},
		{"retry of zero", func(s *zone.SOA) { s.Retry = 0 }, true},

		// RFC 1912 §2.2: a secondary that expires the zone before it has
		// finished retrying drops a zone it could still have recovered.
		{"expire below refresh plus retry", func(s *zone.SOA) { s.Expire = s.Refresh }, true},
		{"expire exactly refresh plus retry", func(s *zone.SOA) { s.Expire = s.Refresh + s.Retry }, true},
		{"expire one above", func(s *zone.SOA) { s.Expire = s.Refresh + s.Retry + 1 }, false},

		// RFC 2181 §8.
		{"refresh above the ttl limit", func(s *zone.SOA) { s.Refresh = zone.MaxTTL + 1 }, true},
		{"minimum above the ttl limit", func(s *zone.SOA) { s.Minimum = zone.MaxTTL + 1 }, true},

		{"minimum of zero is allowed", func(s *zone.SOA) { s.Minimum = 0 }, false},
		{"serial of zero is allowed", func(s *zone.SOA) { s.Serial = zone.NewSerial(0) }, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			soa := testSOA()
			tc.mutate(&soa)

			err := soa.Validate()
			if tc.wantErr {
				if !errors.Is(err, zone.ErrInvalid) {
					t.Fatalf("Validate() = %v, want ErrInvalid", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate(): %v", err)
			}
		})
	}
}

func TestSOARData(t *testing.T) {
	t.Parallel()

	soa := testSOA()
	soa.Serial = zone.NewSerial(42)

	want := "ns1.example.com. hostmaster.example.com. 42 3600 900 1209600 3600"
	if got := soa.RData(); got != want {
		t.Fatalf("RData() = %q, want %q", got, want)
	}

	// The rendered form has to be data the model would accept back, or a
	// zonefile export could not be re-imported.
	parsed, err := zone.ParseRData(zone.TypeSOA, zone.ClassIN, want)
	if err != nil {
		t.Fatalf("the rendered SOA does not parse back: %v", err)
	}
	if parsed.String() != want {
		t.Errorf("re-parsing changed the data: %q", parsed.String())
	}
}

// TestSOANegativeTTL pins RFC 2308 §3: a negative answer is cached for the
// lesser of the SOA record's TTL and the MINIMUM field, so lowering either one
// takes effect.
func TestSOANegativeTTL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ttl     zone.TTL
		minimum zone.TTL
		want    zone.TTL
	}{
		{"minimum is smaller", 3600, 300, 300},
		{"ttl is smaller", 300, 3600, 300},
		{"equal", 3600, 3600, 3600},
		{"minimum of zero", 3600, 0, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			soa := testSOA()
			soa.TTL, soa.Minimum = tc.ttl, tc.minimum
			if got := soa.NegativeTTL(); got != tc.want {
				t.Errorf("NegativeTTL() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestParseSOAData(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		in   string
		want zone.SOA
		bad  bool
	}{
		{
			name: "the form RData writes",
			in:   "ns1.example.com. hostmaster.example.com. 42 3600 900 1209600 3600",
			want: zone.SOA{
				NS: zone.MustParseName("ns1.example.com."), Mbox: zone.MustParseName("hostmaster.example.com."),
				Serial: zone.NewSerial(42), Refresh: 3600, Retry: 900, Expire: 1209600, Minimum: 3600,
			},
		},
		{
			name: "extra whitespace is not a difference",
			in:   "  ns1.example.com.\thostmaster.example.com.   42 3600 900 1209600 3600 ",
			want: zone.SOA{
				NS: zone.MustParseName("ns1.example.com."), Mbox: zone.MustParseName("hostmaster.example.com."),
				Serial: zone.NewSerial(42), Refresh: 3600, Retry: 900, Expire: 1209600, Minimum: 3600,
			},
		},
		{
			name: "a serial at the top of its range",
			in:   "ns1.example. hostmaster.example. 4294967295 3600 900 1209600 3600",
			want: zone.SOA{
				NS: zone.MustParseName("ns1.example."), Mbox: zone.MustParseName("hostmaster.example."),
				Serial: zone.NewSerial(4294967295), Refresh: 3600, Retry: 900, Expire: 1209600, Minimum: 3600,
			},
		},
		{name: "too few values", in: "ns1.example. hostmaster.example. 42 3600 900 1209600", bad: true},
		{name: "too many values", in: "ns1.example. hostmaster.example. 42 3600 900 1209600 3600 7", bad: true},
		{name: "a name that is not one", in: "ns1..example. hostmaster.example. 42 3600 900 1209600 3600", bad: true},
		{name: "a serial past 32 bits", in: "ns1.example. hostmaster.example. 4294967296 3600 900 1209600 3600", bad: true},
		{name: "a negative counter", in: "ns1.example. hostmaster.example. 42 -1 900 1209600 3600", bad: true},
		{name: "a counter past the TTL ceiling", in: "ns1.example. hostmaster.example. 42 2147483648 900 1209600 3600", bad: true},
		{name: "nothing at all", in: "", bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := zone.ParseSOAData(tc.in)
			if tc.bad {
				if err == nil {
					t.Fatalf("parsed %q as %+v, want a refusal", tc.in, got)
				}
				if !errors.Is(err, zone.ErrInvalid) {
					t.Errorf("error = %v, want it to wrap zone.ErrInvalid", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse %q: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("parsed %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestSOADataRoundTrips is the property that matters: what RData writes,
// zone.ParseSOAData reads back unchanged. The journal stores an zone.SOA change as record
// data, so restoring a zone to an earlier state makes that trip.
func TestSOADataRoundTrips(t *testing.T) {
	t.Parallel()

	for _, want := range []zone.SOA{
		zone.DefaultSOA(zone.MustParseName("ns1.example.com."), zone.MustParseName("hostmaster.example.com.")),
		{
			NS: zone.MustParseName("a.b.c.d.e."), Mbox: zone.MustParseName("first\\.last.example."),
			Serial: zone.NewSerial(0), Refresh: 1, Retry: 1, Expire: 1, Minimum: 0,
		},
		{
			NS: zone.MustParseName("ns.example."), Mbox: zone.MustParseName("hostmaster.example."),
			Serial: zone.NewSerial(4294967295), Refresh: zone.MaxTTL, Retry: zone.MaxTTL,
			Expire: zone.MaxTTL, Minimum: zone.MaxTTL,
		},
	} {
		t.Run(want.RData(), func(t *testing.T) {
			t.Parallel()

			got, err := zone.ParseSOAData(want.RData())
			if err != nil {
				t.Fatalf("parse %q: %v", want.RData(), err)
			}
			// The record's own TTL is not part of the data, so it is not read
			// back and is not compared.
			want.TTL = 0
			if got != want {
				t.Errorf("round trip produced %+v, want %+v", got, want)
			}
		})
	}
}
