package zonefile_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/zone"
	"github.com/wegweiserzone/wegweiser/internal/zonefile"
)

// A file with everything RFC 1035 §5 allows a real one to carry: $ORIGIN and
// $TTL, an SOA spread over lines in parentheses, comments, "@" for the apex,
// relative owner names, an owner left off to continue the previous one, and a
// class left off to take IN.
const realistic = `
$ORIGIN example.com.
$TTL 3600

@	IN	SOA	ns1.example.com. hostmaster.example.com. (
			2026081801 ; serial
			7200       ; refresh
			900        ; retry
			1209600    ; expire
			3600 )     ; negative caching

@		IN	NS	ns1.example.com.
		IN	NS	ns2.example.com.
@		IN	MX	10 mail.example.com.

ns1		IN	A	192.0.2.1
ns2		IN	A	192.0.2.2
www		60	IN	A	192.0.2.10
www			IN	AAAA	2001:db8::10
mail		IN	A	192.0.2.20
txt		IN	TXT	"one" "two"
alias		IN	CNAME	www
`

func TestParse(t *testing.T) {
	t.Parallel()

	got, err := zonefile.Parse(strings.NewReader(realistic), zonefile.Options{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if got.Origin.String() != "example.com." {
		t.Errorf("origin = %q, want the name the SOA sits at", got.Origin)
	}

	t.Run("the SOA becomes the zone's settings", func(t *testing.T) {
		if got.SOA.NS.String() != "ns1.example.com." || got.SOA.Mbox.String() != "hostmaster.example.com." {
			t.Errorf("SOA = %+v, want the names from the file", got.SOA)
		}
		// docs/decisions/ D2: an import seeds from this rather than
		// resetting it, so it has to survive the read.
		if got.SOA.Serial.Uint32() != 2026081801 {
			t.Errorf("serial = %d, want the one in the file", got.SOA.Serial.Uint32())
		}
		if got.SOA.Refresh != 7200 || got.SOA.Retry != 900 ||
			got.SOA.Expire != 1209600 || got.SOA.Minimum != 3600 {
			t.Errorf("timers = %+v, want the ones in the file", got.SOA)
		}
		if got.SOA.TTL != 3600 {
			t.Errorf("SOA TTL = %d, want the $TTL default of 3600", got.SOA.TTL)
		}
	})

	t.Run("the SOA is not among the records", func(t *testing.T) {
		// It is the zone's own settings, not a record somebody edits.
		for _, r := range got.Records {
			if r.Type == zone.TypeSOA {
				t.Fatalf("an SOA came back as a record: %s", r)
			}
		}
	})

	t.Run("every line becomes a record", func(t *testing.T) {
		want := []string{
			"example.com.\t3600\tIN\tNS\tns1.example.com.",
			"example.com.\t3600\tIN\tNS\tns2.example.com.",
			"example.com.\t3600\tIN\tMX\t10 mail.example.com.",
			"ns1.example.com.\t3600\tIN\tA\t192.0.2.1",
			"ns2.example.com.\t3600\tIN\tA\t192.0.2.2",
			"www.example.com.\t60\tIN\tA\t192.0.2.10",
			"www.example.com.\t3600\tIN\tAAAA\t2001:db8::10",
			"mail.example.com.\t3600\tIN\tA\t192.0.2.20",
			`txt.example.com.	3600	IN	TXT	"one" "two"`,
			"alias.example.com.\t3600\tIN\tCNAME\twww.example.com.",
		}
		if len(got.Records) != len(want) {
			t.Fatalf("read %d records, want %d:\n%s", len(got.Records), len(want), lines(got.Records))
		}
		for i, w := range want {
			if g := got.Records[i].String(); g != w {
				t.Errorf("record %d\n  got  %q\n  want %q", i, g, w)
			}
		}
	})

	t.Run("the records carry no zone identifier yet", func(t *testing.T) {
		// A file does not know which zone row it will land in, and minting one
		// here would be a second place that invents identifiers.
		for _, r := range got.Records {
			if r.ZoneID != "" || r.ID != "" {
				t.Fatalf("a record arrived with identifiers: %+v", r)
			}
		}
	})
}

func TestParseRefusals(t *testing.T) {
	t.Parallel()

	const soa = "@ IN SOA ns1.example.com. hostmaster.example.com. 1 7200 900 1209600 3600\n" +
		"@ IN NS ns1.example.com.\n"

	for _, tc := range []struct {
		name string
		in   string
		says string
	}{
		{
			name: "no SOA at all",
			in:   "$ORIGIN example.com.\nwww IN A 192.0.2.1\n",
			says: "fragment",
		},
		{
			name: "two SOAs",
			in:   "$ORIGIN example.com.\n" + soa + soa,
			says: "more than one SOA",
		},
		{
			name: "a record outside the zone the SOA names",
			in:   "$ORIGIN example.com.\n" + soa + "www.example.net. IN A 192.0.2.1\n",
			says: "lies outside",
		},
		{
			name: "$INCLUDE, which would read this server's filesystem",
			in:   "$ORIGIN example.com.\n" + soa + "$INCLUDE /etc/passwd\n",
			says: "",
		},
		{
			name: "an SOA written by hand as a record",
			in: "$ORIGIN example.com.\n" + soa +
				"sub IN SOA ns1.example.com. hostmaster.example.com. 1 7200 900 1209600 3600\n",
			says: "more than one SOA",
		},
		{
			name: "data that is not that type",
			in:   "$ORIGIN example.com.\n" + soa + "www IN A not-an-address\n",
			says: "",
		},
		{
			name: "a signed zone, which this server cannot maintain",
			in: "$ORIGIN example.com.\n" + soa +
				"www IN RRSIG A 8 3 3600 20260901000000 20260801000000 1234 example.com. AAAA\n",
			says: "",
		},
		{
			name: "an empty file",
			in:   "",
			says: "fragment",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := zonefile.Parse(strings.NewReader(tc.in), zonefile.Options{})
			if err == nil {
				t.Fatalf("parsed %d records, want a refusal", len(got.Records))
			}
			if !errors.Is(err, zone.ErrInvalid) {
				t.Errorf("error = %v, want it to wrap ErrInvalid", err)
			}
			if tc.says != "" && !strings.Contains(err.Error(), tc.says) {
				t.Errorf("error = %q, want it to mention %q", err, tc.says)
			}
			if strings.Contains(err.Error(), "dns:") {
				t.Errorf("error = %q, names a library the operator never chose", err)
			}
		})
	}
}

func TestParseOptions(t *testing.T) {
	t.Parallel()

	t.Run("an origin supplied for a file that sets none", func(t *testing.T) {
		const rel = "@ IN SOA ns1 hostmaster 1 7200 900 1209600 3600\n" +
			"@ IN NS ns1\nwww IN A 192.0.2.1\n"

		got, err := zonefile.Parse(strings.NewReader(rel), zonefile.Options{
			Origin:     zone.MustParseName("example.com."),
			DefaultTTL: 300,
		})
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if got.Origin.String() != "example.com." {
			t.Errorf("origin = %q", got.Origin)
		}
		if got.Records[1].Name.String() != "www.example.com." {
			t.Errorf("relative name became %q", got.Records[1].Name)
		}
		if got.Records[1].TTL != 300 {
			t.Errorf("ttl = %d, want the supplied default", got.Records[1].TTL)
		}
	})

	t.Run("a file that expands past the bound", func(t *testing.T) {
		// $GENERATE is thirty octets and can become sixteen million records,
		// so the bound is on what a file becomes rather than on its length.
		const gen = "$ORIGIN example.com.\n" +
			"@ IN SOA ns1.example.com. hostmaster.example.com. 1 7200 900 1209600 3600\n" +
			"@ IN NS ns1.example.com.\n" +
			"$GENERATE 1-5000 host-$ IN A 192.0.2.1\n"

		if _, err := zonefile.Parse(strings.NewReader(gen), zonefile.Options{MaxRecords: 100}); err == nil {
			t.Fatal("a file past the bound was read in full")
		}

		got, err := zonefile.Parse(strings.NewReader(gen), zonefile.Options{MaxRecords: 10000})
		if err != nil {
			t.Fatalf("parse within the bound: %v", err)
		}
		if len(got.Records) != 5001 {
			t.Errorf("read %d records, want the NS and five thousand generated", len(got.Records))
		}
	})
}

func lines(recs []zone.Record) string {
	var b strings.Builder
	for i := range recs {
		b.WriteString("  ")
		b.WriteString(recs[i].String())
		b.WriteByte('\n')
	}
	return b.String()
}
