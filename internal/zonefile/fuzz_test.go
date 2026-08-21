package zonefile_test

import (
	"strings"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/zone"
	"github.com/wegweiserzone/wegweiser/internal/zonefile"
)

// FuzzParse checks that no file makes the reader panic, and that anything it
// does accept is something the rest of the server would accept too.
func FuzzParse(f *testing.F) {
	f.Add(realistic)
	f.Add("")
	f.Add("$ORIGIN example.com.\n@ IN SOA ns1.example.com. hostmaster.example.com. 1 7200 900 1209600 3600\n")
	f.Add("$TTL 300\n$ORIGIN .\n@ IN SOA a. b. 1 7200 900 1209600 3600\n@ IN NS a.\n")
	f.Add("@ IN SOA ns1 hostmaster 1 7200 900 1209600 3600\nwww IN A 192.0.2.1\n")
	f.Add("$ORIGIN example.com.\n@ IN SOA ns1.example.com. hostmaster.example.com. 1 7200 900 1209600 3600\n" +
		"* IN A 192.0.2.1\n_sip._tcp IN SRV 0 5 5060 sip.example.com.\n" +
		`x IN TYPE65535 \# 4 01020304` + "\n")
	f.Add("$ORIGIN example.com.\n@ IN SOA ns1.example.com. hostmaster.example.com. 1 7200 900 1209600 3600\n(((")
	f.Add("$INCLUDE /etc/passwd\n")
	f.Add("$GENERATE 1-100 host-$ IN A 192.0.2.1\n")

	f.Fuzz(func(t *testing.T, in string) {
		// Bounded, because the fuzzer will find $GENERATE and a fuzz target
		// that allocates a million records per case measures the allocator.
		got, err := zonefile.Parse(strings.NewReader(in), zonefile.Options{
			Origin:     zone.MustParseName("example.com."),
			MaxRecords: 1000,
		})
		if err != nil {
			if got != nil {
				t.Fatalf("a refusal came back with content: %v", err)
			}
			return
		}

		if got.Origin.IsZero() {
			t.Fatal("accepted a file without saying which zone it describes")
		}
		if err := got.SOA.Validate(); err != nil {
			t.Fatalf("accepted an SOA the model refuses: %v", err)
		}

		for i := range got.Records {
			r := &got.Records[i]
			if err := r.Validate(); err != nil {
				t.Fatalf("accepted a record the model refuses: %s: %v", r, err)
			}
			if !r.Name.IsSubDomainOf(got.Origin) {
				t.Fatalf("accepted %q, which lies outside %q", r.Name, got.Origin)
			}
			// The data came back through the library's own printer, so it has
			// to survive a second trip through its parser unchanged. If it
			// does not, what was stored is not what the wire will carry.
			again, err := zone.ParseRData(r.Type, r.Class, r.RData.String())
			if err != nil {
				t.Fatalf("accepted data that does not parse back: %s: %v", r, err)
			}
			if !again.Equal(r.RData) {
				t.Fatalf("data changed on a second trip\n  first  %q\n  second %q",
					r.RData, again)
			}
		}
	})
}
