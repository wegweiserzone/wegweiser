package dns

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	wire "github.com/miekg/dns"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// The SOA every negative answer from these fixtures carries. With the default
// parameters the negative TTL of RFC 2308 §3 is the SOA's own TTL, so the two
// forms of the record look alike; TestResolveNegativeTTL takes them apart.
const soaExampleCom = "example.com. 3600 IN SOA ns1.example.com. hostmaster.example.com. " +
	"1 3600 900 1209600 3600"

// lines renders a section the way the table below writes it: one record per
// string, fields separated by single spaces rather than by the tabs the wire
// library prints.
func lines(rrs []wire.RR) []string {
	out := make([]string, 0, len(rrs))
	for _, rr := range rrs {
		out = append(out, strings.Join(strings.Fields(rr.String()), " "))
	}
	return out
}

// snapshot builds a snapshot from whole zones, each given as its apex followed
// by its records.
func snapshot(t testing.TB, zones map[string][]string) *Snapshot {
	t.Helper()

	list := make([]*zone.Zone, 0, len(zones))
	src := source{records: make(map[zone.ZoneID][]*zone.Record, len(zones))}
	for apex, lines := range zones {
		z := newZone(t, apex)
		list = append(list, z)
		src.records[z.ID] = records(t, z.ID, lines...)
	}

	snap, err := Build(t.Context(), list, src)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return snap
}

// resolveFixture is one snapshot broad enough to hold every shape the search of
// RFC 1034 §4.3.2 has to tell apart, so that the table below reads as a list of
// questions rather than as a list of zones.
func resolveFixture(t testing.TB) *Snapshot {
	t.Helper()
	return snapshot(t, map[string][]string{
		"example.com.": {
			"example.com. 3600 IN NS ns1.example.com.",
			"example.com. 3600 IN NS ns2.example.com.",
			"ns1.example.com. 3600 IN A 192.0.2.1",
			"ns2.example.com. 3600 IN A 192.0.2.2",

			"www.example.com. 300 IN A 192.0.2.10",
			"www.example.com. 300 IN A 192.0.2.11",
			"www.example.com. 300 IN AAAA 2001:db8::10",
			"txt.example.com. 300 IN TXT \"hello\"",
			"mail.example.com. 300 IN MX 10 mx.example.com.",
			"mx.example.com. 300 IN A 192.0.2.50",

			// An empty non-terminal, two levels of it.
			"host.deep.sub.example.com. 300 IN A 192.0.2.20",

			// A wildcard, and below it a name that shadows it for its own
			// subtree.
			"*.wild.example.com. 300 IN A 192.0.2.30",
			"host.deep.wild.example.com. 300 IN A 192.0.2.31",
			"*.cwild.example.com. 300 IN CNAME www.example.com.",

			// A delegation, with glue and with a name below it that this zone
			// holds but is not allowed to answer with.
			"sub2.example.com. 3600 IN NS ns1.sub2.example.com.",
			"ns1.sub2.example.com. 3600 IN A 192.0.2.40",
			"deep.sub2.example.com. 300 IN A 192.0.2.41",

			"alias.example.com. 300 IN CNAME www.example.com.",
			"out.example.com. 300 IN CNAME elsewhere.invalid.",
			"dangling.example.com. 300 IN CNAME nothere.example.com.",
			"tonodata.example.com. 300 IN CNAME txt.example.com.",
			"cross.example.com. 300 IN CNAME www.example.net.",
			"loop1.example.com. 300 IN CNAME loop2.example.com.",
			"loop2.example.com. 300 IN CNAME loop1.example.com.",
		},
		"example.net.": {
			"www.example.net. 300 IN A 198.51.100.5",
		},
	})
}

func TestResolve(t *testing.T) {
	t.Parallel()
	snap := resolveFixture(t)

	tests := []struct {
		name  string
		qname string
		qtype zone.RRType

		rcode      int
		aa         bool
		answer     []string
		authority  []string
		additional []string
		ede        bool
		edeCode    uint16
	}{
		// Exact matches (RFC 1034 §4.3.2 step 3a).
		{
			name: "an RRset is answered whole", qname: "www.example.com.", qtype: zone.TypeA,
			aa: true,
			answer: []string{
				"www.example.com. 300 IN A 192.0.2.10",
				"www.example.com. 300 IN A 192.0.2.11",
			},
		},
		{
			name: "a second type at the same name", qname: "www.example.com.", qtype: zone.TypeAAAA,
			aa:     true,
			answer: []string{"www.example.com. 300 IN AAAA 2001:db8::10"},
		},
		{
			name:  "the SOA is answered from the zone's own parameters",
			qname: "example.com.", qtype: zone.TypeSOA,
			aa:     true,
			answer: []string{soaExampleCom},
		},
		{
			name:  "NS at the apex is data, not a delegation",
			qname: "example.com.", qtype: zone.TypeNS,
			aa: true,
			answer: []string{
				"example.com. 3600 IN NS ns1.example.com.",
				"example.com. 3600 IN NS ns2.example.com.",
			},
			additional: []string{
				"ns1.example.com. 3600 IN A 192.0.2.1",
				"ns2.example.com. 3600 IN A 192.0.2.2",
			},
		},
		{
			name:  "the query name is matched without regard to case (RFC 4343)",
			qname: "WWW.Example.COM.", qtype: zone.TypeAAAA,
			aa:     true,
			answer: []string{"www.example.com. 300 IN AAAA 2001:db8::10"},
		},

		// NODATA and empty non-terminals.
		{
			name:  "a name without the type asked for is NODATA",
			qname: "www.example.com.", qtype: zone.TypeMX,
			aa: true, authority: []string{soaExampleCom},
		},
		{
			name:  "an empty non-terminal is NODATA, not NXDOMAIN",
			qname: "sub.example.com.", qtype: zone.TypeA,
			aa: true, authority: []string{soaExampleCom},
		},
		{
			name:  "the second level of an empty non-terminal is NODATA too",
			qname: "deep.sub.example.com.", qtype: zone.TypeA,
			aa: true, authority: []string{soaExampleCom},
		},

		// NXDOMAIN.
		{
			name:  "a name that is nowhere is NXDOMAIN",
			qname: "nothere.example.com.", qtype: zone.TypeA,
			rcode: wire.RcodeNameError, aa: true, authority: []string{soaExampleCom},
		},
		{
			name:  "a name below one that exists is still NXDOMAIN",
			qname: "x.www.example.com.", qtype: zone.TypeA,
			rcode: wire.RcodeNameError, aa: true, authority: []string{soaExampleCom},
		},

		// Wildcards (RFC 4592).
		{
			name:  "a wildcard answers under the name that was asked for",
			qname: "x.wild.example.com.", qtype: zone.TypeA,
			aa:     true,
			answer: []string{"x.wild.example.com. 300 IN A 192.0.2.30"},
		},
		{
			name:  "a wildcard reaches more than one label below its parent",
			qname: "a.b.wild.example.com.", qtype: zone.TypeA,
			aa:     true,
			answer: []string{"a.b.wild.example.com. 300 IN A 192.0.2.30"},
		},
		{
			name:  "an existing name shadows a wildcard above it",
			qname: "x.deep.wild.example.com.", qtype: zone.TypeA,
			rcode: wire.RcodeNameError, aa: true, authority: []string{soaExampleCom},
		},
		{
			name:  "a wildcard without the type asked for is NODATA",
			qname: "x.wild.example.com.", qtype: zone.TypeAAAA,
			aa: true, authority: []string{soaExampleCom},
		},
		{
			name:  "a synthesised CNAME is chased like any other",
			qname: "x.cwild.example.com.", qtype: zone.TypeA,
			aa: true,
			answer: []string{
				"x.cwild.example.com. 300 IN CNAME www.example.com.",
				"www.example.com. 300 IN A 192.0.2.10",
				"www.example.com. 300 IN A 192.0.2.11",
			},
		},

		// Delegations (RFC 1034 §4.3.2 step 3b).
		{
			name:  "the delegation point itself is referred, not answered",
			qname: "sub2.example.com.", qtype: zone.TypeA,
			answer:     nil,
			authority:  []string{"sub2.example.com. 3600 IN NS ns1.sub2.example.com."},
			additional: []string{"ns1.sub2.example.com. 3600 IN A 192.0.2.40"},
		},
		{
			name:  "a question for the delegation's NS is referred as well",
			qname: "sub2.example.com.", qtype: zone.TypeNS,
			authority:  []string{"sub2.example.com. 3600 IN NS ns1.sub2.example.com."},
			additional: []string{"ns1.sub2.example.com. 3600 IN A 192.0.2.40"},
		},
		{
			name:  "data below a delegation is not ours to answer with",
			qname: "deep.sub2.example.com.", qtype: zone.TypeA,
			authority:  []string{"sub2.example.com. 3600 IN NS ns1.sub2.example.com."},
			additional: []string{"ns1.sub2.example.com. 3600 IN A 192.0.2.40"},
		},
		{
			name:  "a name that does not exist below a delegation is referred, not denied",
			qname: "a.b.sub2.example.com.", qtype: zone.TypeA,
			authority:  []string{"sub2.example.com. 3600 IN NS ns1.sub2.example.com."},
			additional: []string{"ns1.sub2.example.com. 3600 IN A 192.0.2.40"},
		},

		// CNAME chains (RFC 1034 §4.3.2 step 3a).
		{
			name:  "a CNAME is followed inside the zone",
			qname: "alias.example.com.", qtype: zone.TypeA,
			aa: true,
			answer: []string{
				"alias.example.com. 300 IN CNAME www.example.com.",
				"www.example.com. 300 IN A 192.0.2.10",
				"www.example.com. 300 IN A 192.0.2.11",
			},
		},
		{
			name:  "a question for the CNAME itself is not chased",
			qname: "alias.example.com.", qtype: zone.TypeCNAME,
			aa:     true,
			answer: []string{"alias.example.com. 300 IN CNAME www.example.com."},
		},
		{
			name:  "a CNAME out of our authority ends the answer",
			qname: "out.example.com.", qtype: zone.TypeA,
			aa:     true,
			answer: []string{"out.example.com. 300 IN CNAME elsewhere.invalid."},
		},
		{
			name:  "a CNAME into another zone we serve is followed across",
			qname: "cross.example.com.", qtype: zone.TypeA,
			aa: true,
			answer: []string{
				"cross.example.com. 300 IN CNAME www.example.net.",
				"www.example.net. 300 IN A 198.51.100.5",
			},
		},
		{
			// RFC 2308 §2.1: the chain stays in the answer and the RCODE
			// describes where it ended.
			name:  "a CNAME to a name that does not exist is NXDOMAIN with the chain",
			qname: "dangling.example.com.", qtype: zone.TypeA,
			rcode: wire.RcodeNameError, aa: true,
			answer:    []string{"dangling.example.com. 300 IN CNAME nothere.example.com."},
			authority: []string{soaExampleCom},
		},
		{
			name:  "a CNAME to a name without the type is NODATA with the chain",
			qname: "tonodata.example.com.", qtype: zone.TypeA,
			aa:        true,
			answer:    []string{"tonodata.example.com. 300 IN CNAME txt.example.com."},
			authority: []string{soaExampleCom},
		},
		{
			name:  "a CNAME loop is our fault and is reported as one",
			qname: "loop1.example.com.", qtype: zone.TypeA,
			rcode: wire.RcodeServerFailure,
			ede:   true, edeCode: wire.ExtendedErrorCodeOther,
		},

		// QTYPE=ANY, answered minimally (RFC 8482 §4.1).
		{
			name:  "ANY answers with one RRset, the lowest type at the name",
			qname: "www.example.com.", qtype: zone.TypeANY,
			aa: true,
			answer: []string{
				"www.example.com. 300 IN A 192.0.2.10",
				"www.example.com. 300 IN A 192.0.2.11",
			},
		},
		{
			name:  "ANY at the apex answers with one RRset as well",
			qname: "example.com.", qtype: zone.TypeANY,
			aa: true,
			answer: []string{
				"example.com. 3600 IN NS ns1.example.com.",
				"example.com. 3600 IN NS ns2.example.com.",
			},
			additional: []string{
				"ns1.example.com. 3600 IN A 192.0.2.1",
				"ns2.example.com. 3600 IN A 192.0.2.2",
			},
		},
		{
			name:  "ANY at a CNAME answers the CNAME and does not chase it",
			qname: "alias.example.com.", qtype: zone.TypeANY,
			aa:     true,
			answer: []string{"alias.example.com. 300 IN CNAME www.example.com."},
		},
		{
			name:  "ANY at an empty non-terminal is NODATA",
			qname: "sub.example.com.", qtype: zone.TypeANY,
			aa: true, authority: []string{soaExampleCom},
		},

		// Everything outside what this server answers.
		{
			name:  "a name in no zone we serve is refused, never denied",
			qname: "www.example.org.", qtype: zone.TypeA,
			rcode: wire.RcodeRefused,
			ede:   true, edeCode: wire.ExtendedErrorCodeNotAuthoritative,
		},
		{
			name:  "a transfer is not a question this server answers",
			qname: "example.com.", qtype: zone.TypeAXFR,
			rcode: wire.RcodeNotImplemented,
			ede:   true, edeCode: wire.ExtendedErrorCodeNotSupported,
		},
		{
			name:  "a meta type is not a question at all",
			qname: "example.com.", qtype: zone.TypeOPT,
			rcode: wire.RcodeNotImplemented,
			ede:   true, edeCode: wire.ExtendedErrorCodeNotSupported,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var got Answer
			snap.Resolve(Question{
				Name:  zone.MustParseName(tt.qname),
				Class: zone.ClassIN,
				Type:  tt.qtype,
			}, &got)

			if got.Rcode != tt.rcode {
				t.Errorf("rcode = %s, want %s",
					wire.RcodeToString[got.Rcode], wire.RcodeToString[tt.rcode])
			}
			if got.Authoritative != tt.aa {
				t.Errorf("AA = %v, want %v", got.Authoritative, tt.aa)
			}
			if answer := lines(got.Answer); !slices.Equal(answer, tt.answer) {
				t.Errorf("answer section:\n got %q\nwant %q", answer, tt.answer)
			}
			if authority := lines(got.Authority); !slices.Equal(authority, tt.authority) {
				t.Errorf("authority section:\n got %q\nwant %q", authority, tt.authority)
			}
			if additional := lines(got.Additional); !slices.Equal(additional, tt.additional) {
				t.Errorf("additional section:\n got %q\nwant %q", additional, tt.additional)
			}
			if got.Extended.Present != tt.ede {
				t.Errorf("extended error present = %v, want %v", got.Extended.Present, tt.ede)
			}
			if tt.ede && got.Extended.Code != tt.edeCode {
				t.Errorf("extended error = %d (%s), want %d (%s)",
					got.Extended.Code, wire.ExtendedErrorCodeToString[got.Extended.Code],
					tt.edeCode, wire.ExtendedErrorCodeToString[tt.edeCode])
			}
			if tt.ede && got.Extended.Text == "" {
				t.Error("extended error carries no text, which is the point of having one")
			}
		})
	}
}

// TestResolveAdditional covers step 6 of RFC 1034 §4.3.2: the addresses a
// resolver would otherwise have to ask for separately, and the ones it must not
// be handed because they are not ours to give.
func TestResolveAdditional(t *testing.T) {
	t.Parallel()

	snap := snapshot(t, map[string][]string{
		"example.com.": {
			"example.com. 3600 IN NS ns1.example.com.",
			"example.com. 3600 IN NS ns2.example.com.",
			"ns1.example.com. 3600 IN A 192.0.2.1",
			"ns1.example.com. 3600 IN AAAA 2001:db8::1",
			"ns2.example.com. 3600 IN A 192.0.2.2",

			"example.com. 300 IN MX 10 mx.example.com.",
			"mx.example.com. 300 IN A 192.0.2.5",
			"nowhere.example.com. 300 IN MX 10 mx.example.org.",
			"reach.example.com. 300 IN MX 10 mx.sub.example.com.",
			"_sip._tcp.example.com. 300 IN SRV 0 5 5060 sip.example.com.",
			"sip.example.com. 300 IN A 192.0.2.6",
			"*.wild.example.com. 300 IN MX 10 mx.example.com.",

			// A delegation whose name servers live inside it. Their addresses
			// are glue for this delegation and for nothing else.
			"sub.example.com. 3600 IN NS ns1.sub.example.com.",
			"ns1.sub.example.com. 3600 IN A 192.0.2.10",
		},
		// A second zone we serve, to show that an MX target is not completed
		// across zones even when we could.
		"example.org.": {"mx.example.org. 300 IN A 198.51.100.1"},
	})

	for _, tt := range []struct {
		name       string
		qname      string
		qtype      zone.RRType
		additional []string
	}{
		{
			name:  "the name servers of a referral bring their glue",
			qname: "www.sub.example.com.", qtype: zone.TypeA,
			additional: []string{"ns1.sub.example.com. 3600 IN A 192.0.2.10"},
		},
		{
			name:  "apex name servers bring every address they have",
			qname: "example.com.", qtype: zone.TypeNS,
			additional: []string{
				"ns1.example.com. 3600 IN A 192.0.2.1",
				"ns1.example.com. 3600 IN AAAA 2001:db8::1",
				"ns2.example.com. 3600 IN A 192.0.2.2",
			},
		},
		{
			name:  "an MX brings the address of its target",
			qname: "example.com.", qtype: zone.TypeMX,
			additional: []string{"mx.example.com. 300 IN A 192.0.2.5"},
		},
		{
			name:  "an SRV brings the address of its target",
			qname: "_sip._tcp.example.com.", qtype: zone.TypeSRV,
			additional: []string{"sip.example.com. 300 IN A 192.0.2.6"},
		},
		{
			name:  "a synthesised MX brings its target's address too",
			qname: "x.wild.example.com.", qtype: zone.TypeMX,
			additional: []string{"mx.example.com. 300 IN A 192.0.2.5"},
		},
		{
			name:  "a target in another zone we serve is still not completed",
			qname: "nowhere.example.com.", qtype: zone.TypeMX,
			additional: nil,
		},
		{
			// The address exists in this zone, as glue for sub.example.com. It
			// is not ours to hand out as the answer to something else.
			name:  "an address below a delegation is not glue for anything else",
			qname: "reach.example.com.", qtype: zone.TypeMX,
			additional: nil,
		},
		{
			name:  "an answer that points nowhere brings nothing",
			qname: "example.com.", qtype: zone.TypeSOA,
			additional: nil,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var got Answer
			snap.Resolve(Question{
				Name: zone.MustParseName(tt.qname), Class: zone.ClassIN, Type: tt.qtype,
			}, &got)

			if additional := lines(got.Additional); !slices.Equal(additional, tt.additional) {
				t.Errorf("additional section:\n got %q\nwant %q", additional, tt.additional)
			}
		})
	}
}

// TestResolveAdditionalLimit pins the bound of architecture §2.8. An
// answer with more targets than the section holds is filled up to the limit and
// stops there, with every RRset it did take left whole.
func TestResolveAdditionalLimit(t *testing.T) {
	t.Parallel()

	lines := []string{}
	for i := range maxAdditional + 4 {
		lines = append(lines,
			fmt.Sprintf("many.example.com. 300 IN MX %d mx%d.example.com.", i, i),
			fmt.Sprintf("mx%d.example.com. 300 IN A 192.0.2.%d", i, i+1),
		)
	}
	snap := snapshot(t, map[string][]string{"example.com.": lines})

	var got Answer
	snap.Resolve(Question{
		Name:  zone.MustParseName("many.example.com."),
		Class: zone.ClassIN,
		Type:  zone.TypeMX,
	}, &got)

	if len(got.Answer) != maxAdditional+4 {
		t.Errorf("answer holds %d records, want all %d", len(got.Answer), maxAdditional+4)
	}
	if len(got.Additional) != maxAdditional {
		t.Errorf("additional holds %d records, want the limit of %d",
			len(got.Additional), maxAdditional)
	}
}

// TestResolveCNAMEChainLength pins the bound of architecture §2.8 from
// both sides, because a limit that is only tested from the far side is a limit
// that can be off by one without anyone noticing.
func TestResolveCNAMEChainLength(t *testing.T) {
	t.Parallel()

	zoneLines := []string{"end.chain.example. 300 IN A 192.0.2.1"}
	// c1..c8 is a chain of exactly maxCNAMEChain records; d1..d9 is one longer.
	for _, c := range []struct {
		label  string
		length int
	}{{"c", maxCNAMEChain}, {"d", maxCNAMEChain + 1}} {
		for i := 1; i <= c.length; i++ {
			target := fmt.Sprintf("%s%d.chain.example.", c.label, i+1)
			if i == c.length {
				target = "end.chain.example."
			}
			zoneLines = append(zoneLines,
				fmt.Sprintf("%s%d.chain.example. 300 IN CNAME %s", c.label, i, target))
		}
	}
	snap := snapshot(t, map[string][]string{"chain.example.": zoneLines})

	for _, tt := range []struct {
		name   string
		qname  string
		rcode  int
		length int
	}{
		{"a chain of exactly the limit is answered", "c1.chain.example.",
			wire.RcodeSuccess, maxCNAMEChain + 1},
		{"one CNAME more is refused as broken data", "d1.chain.example.",
			wire.RcodeServerFailure, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var got Answer
			snap.Resolve(Question{
				Name:  zone.MustParseName(tt.qname),
				Class: zone.ClassIN,
				Type:  zone.TypeA,
			}, &got)

			if got.Rcode != tt.rcode {
				t.Errorf("rcode = %s, want %s",
					wire.RcodeToString[got.Rcode], wire.RcodeToString[tt.rcode])
			}
			if len(got.Answer) != tt.length {
				t.Errorf("answer holds %d records, want %d: %q",
					len(got.Answer), tt.length, lines(got.Answer))
			}
		})
	}
}

// TestResolveNegativeTTL is the reason a zone carries two SOA records: the one
// it answers with and the shorter one RFC 2308 §3 puts on a denial.
func TestResolveNegativeTTL(t *testing.T) {
	t.Parallel()

	z := newZone(t, "short.example.")
	z.SOA.TTL = 3600
	z.SOA.Minimum = 60
	src := source{records: map[zone.ZoneID][]*zone.Record{
		z.ID: records(t, z.ID, "www.short.example. 300 IN A 192.0.2.1"),
	}}
	snap, err := Build(t.Context(), []*zone.Zone{z}, src)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	var answered Answer
	snap.Resolve(Question{
		Name: z.Name, Class: zone.ClassIN, Type: zone.TypeSOA,
	}, &answered)
	if got, want := answered.Answer[0].Header().Ttl, uint32(3600); got != want {
		t.Errorf("the answered SOA has TTL %d, want the record's own %d", got, want)
	}

	var denied Answer
	snap.Resolve(Question{
		Name: zone.MustParseName("nothere.short.example."), Class: zone.ClassIN, Type: zone.TypeA,
	}, &denied)
	if got, want := denied.Authority[0].Header().Ttl, uint32(60); got != want {
		t.Errorf("the SOA on a denial has TTL %d, want min(TTL, MINIMUM) = %d", got, want)
	}

	// The two must be separate records: writing the negative TTL into the one
	// the apex answers with would corrupt every concurrent query for the SOA.
	if answered.Answer[0] == denied.Authority[0] {
		t.Error("the answered SOA and the negative SOA are the same record")
	}
}

// TestResolveZoneSelection covers the walk of architecture §2.4: the
// most specific zone wins, and a name in none of them is refused.
func TestResolveZoneSelection(t *testing.T) {
	t.Parallel()

	snap := snapshot(t, map[string][]string{
		"example.org.":       {"www.example.org. 300 IN A 192.0.2.1"},
		"child.example.org.": {"www.child.example.org. 300 IN A 192.0.2.2"},
	})

	for _, tt := range []struct {
		name   string
		qname  string
		rcode  int
		answer []string
	}{
		{"the parent zone answers its own names", "www.example.org.",
			wire.RcodeSuccess, []string{"www.example.org. 300 IN A 192.0.2.1"}},
		{"the child zone answers below its apex", "www.child.example.org.",
			wire.RcodeSuccess, []string{"www.child.example.org. 300 IN A 192.0.2.2"}},
		{"a name in neither is refused", "www.example.com.", wire.RcodeRefused, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var got Answer
			snap.Resolve(Question{
				Name: zone.MustParseName(tt.qname), Class: zone.ClassIN, Type: zone.TypeA,
			}, &got)

			if got.Rcode != tt.rcode {
				t.Errorf("rcode = %s, want %s",
					wire.RcodeToString[got.Rcode], wire.RcodeToString[tt.rcode])
			}
			if answer := lines(got.Answer); !slices.Equal(answer, tt.answer) {
				t.Errorf("answer section:\n got %q\nwant %q", answer, tt.answer)
			}
		})
	}
}

// TestResolveRootZone checks that nothing in the walk assumes a name has a
// parent: a server holding the root has an apex with none.
func TestResolveRootZone(t *testing.T) {
	t.Parallel()

	snap := snapshot(t, map[string][]string{".": {"example. 300 IN A 192.0.2.1"}})

	for _, tt := range []struct {
		name   string
		qname  string
		qtype  zone.RRType
		rcode  int
		answer []string
	}{
		{"a name under the root", "example.", zone.TypeA,
			wire.RcodeSuccess, []string{"example. 300 IN A 192.0.2.1"}},
		{"the root itself", ".", zone.TypeSOA, wire.RcodeSuccess,
			[]string{". 3600 IN SOA ns1. hostmaster. 1 3600 900 1209600 3600"}},
		{"a name the root zone does not hold", "nothere.", zone.TypeA,
			wire.RcodeNameError, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var got Answer
			snap.Resolve(Question{
				Name: zone.MustParseName(tt.qname), Class: zone.ClassIN, Type: tt.qtype,
			}, &got)

			if got.Rcode != tt.rcode {
				t.Errorf("rcode = %s, want %s",
					wire.RcodeToString[got.Rcode], wire.RcodeToString[tt.rcode])
			}
			if answer := lines(got.Answer); !slices.Equal(answer, tt.answer) {
				t.Errorf("answer section:\n got %q\nwant %q", answer, tt.answer)
			}
		})
	}
}

// TestResolveClass records what the resolver does with a class it holds no
// records in. The question's class is carried through every lookup rather than
// assumed to be IN, so a CH question finds the name and not the data: NODATA,
// not a wrong answer. In v0.1 nothing but IN reaches here, because the message
// layer refuses the rest (architecture §2.2); this pins the behaviour
// for the day that changes.
func TestResolveClass(t *testing.T) {
	t.Parallel()

	snap := resolveFixture(t)

	for _, qtype := range []zone.RRType{zone.TypeA, zone.TypeANY} {
		t.Run(qtype.String(), func(t *testing.T) {
			t.Parallel()

			var got Answer
			snap.Resolve(Question{
				Name: zone.MustParseName("www.example.com."), Class: zone.ClassCH, Type: qtype,
			}, &got)

			if got.Rcode != wire.RcodeSuccess {
				t.Errorf("rcode = %s, want NOERROR", wire.RcodeToString[got.Rcode])
			}
			if len(got.Answer) != 0 {
				t.Errorf("answer section = %q, want an IN record not to answer a CH question",
					lines(got.Answer))
			}
			if authority := lines(got.Authority); !slices.Equal(authority, []string{soaExampleCom}) {
				t.Errorf("authority section = %q, want the SOA", authority)
			}
		})
	}
}

// TestResolveNilSnapshot covers a server wired up before its first build: it
// answers for nothing rather than panicking on the query path.
func TestResolveNilSnapshot(t *testing.T) {
	t.Parallel()

	var snap *Snapshot
	var got Answer
	snap.Resolve(Question{
		Name: zone.MustParseName("www.example.com."), Class: zone.ClassIN, Type: zone.TypeA,
	}, &got)

	if got.Rcode != wire.RcodeRefused {
		t.Errorf("rcode = %s, want REFUSED", wire.RcodeToString[got.Rcode])
	}
}

// TestResolveHandAssembledCNAME covers the fallback for a CNAME whose target
// was never parsed. The builder parses one for every CNAME it stores, so this
// shape only arises from a tree assembled in code, but the query path indexes
// that slice, and an index is worth a guard when the alternative is a panic on
// the hot path.
func TestResolveHandAssembledCNAME(t *testing.T) {
	t.Parallel()

	snap := snapshot(t, map[string][]string{"hand.example.": {}})
	tr := snap.zoneAt(zone.MustParseName("hand.example."))
	if tr == nil {
		t.Fatal("no zone for hand.example.")
	}

	name := zone.MustParseName("alias.hand.example.")
	rr, err := wire.NewRR("alias.hand.example. 300 IN CNAME www.hand.example.")
	if err != nil {
		t.Fatalf("new RR: %v", err)
	}
	tr.node(name).add(zone.ClassIN, zone.TypeCNAME, rr)

	var got Answer
	snap.Resolve(Question{Name: name, Class: zone.ClassIN, Type: zone.TypeA}, &got)

	if got.Rcode != wire.RcodeSuccess {
		t.Errorf("rcode = %s, want NOERROR", wire.RcodeToString[got.Rcode])
	}
	want := []string{"alias.hand.example. 300 IN CNAME www.hand.example."}
	if answer := lines(got.Answer); !slices.Equal(answer, want) {
		t.Errorf("answer section:\n got %q\nwant %q", answer, want)
	}
}

// TestAnswerReset checks that a reused answer carries nothing over from the
// query before it, which is the whole basis of handing one back per worker.
func TestAnswerReset(t *testing.T) {
	t.Parallel()

	snap := resolveFixture(t)
	var a Answer

	snap.Resolve(Question{
		Name: zone.MustParseName("www.example.org."), Class: zone.ClassIN, Type: zone.TypeA,
	}, &a)
	if !a.Extended.Present {
		t.Fatal("the refused answer carries no extended error to be cleared")
	}

	snap.Resolve(Question{
		Name: zone.MustParseName("www.example.com."), Class: zone.ClassIN, Type: zone.TypeA,
	}, &a)

	if a.Rcode != wire.RcodeSuccess || !a.Authoritative {
		t.Errorf("rcode = %s, AA = %v, want NOERROR and AA set",
			wire.RcodeToString[a.Rcode], a.Authoritative)
	}
	if a.Extended.Present {
		t.Errorf("the extended error of the previous query survived: %+v", a.Extended)
	}
	if len(a.Answer) != 2 || len(a.Authority) != 0 {
		t.Errorf("sections hold %d answer and %d authority records, want 2 and 0",
			len(a.Answer), len(a.Authority))
	}
}

// TestResolveAllocations is the per-query half of the allocation target in D12.
// Wildcard synthesis is the deliberate exception (it has to rewrite an owner
// name, and rewriting a shared record means copying it) so it is measured here
// rather than left to be discovered later.
func TestResolveAllocations(t *testing.T) {
	snap := resolveFixture(t)

	for _, tt := range []struct {
		name  string
		qname string
		qtype zone.RRType
		want  float64
	}{
		{"exact match", "www.example.com.", zone.TypeA, 0},
		{"NODATA", "www.example.com.", zone.TypeMX, 0},
		{"empty non-terminal", "sub.example.com.", zone.TypeA, 0},
		{"NXDOMAIN", "nothere.example.com.", zone.TypeA, 0},
		{"referral with glue", "deep.sub2.example.com.", zone.TypeA, 0},
		{"MX with its target's address", "mail.example.com.", zone.TypeMX, 0},
		{"refused", "www.example.org.", zone.TypeA, 0},
		{"CNAME chased across zones", "cross.example.com.", zone.TypeA, 0},
		{"ANY", "www.example.com.", zone.TypeANY, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			q := Question{
				Name: zone.MustParseName(tt.qname), Class: zone.ClassIN, Type: tt.qtype,
			}
			var a Answer
			if got := testing.AllocsPerRun(100, func() { snap.Resolve(q, &a) }); got != tt.want {
				t.Errorf("%.0f allocations per query, want %.0f", got, tt.want)
			}
		})
	}

	t.Run("wildcard synthesis copies", func(t *testing.T) {
		q := Question{
			Name:  zone.MustParseName("x.wild.example.com."),
			Class: zone.ClassIN,
			Type:  zone.TypeA,
		}
		var a Answer
		if got := testing.AllocsPerRun(100, func() { snap.Resolve(q, &a) }); got == 0 {
			t.Error("wildcard synthesis allocated nothing, so it is no longer copying " +
				"the record it rewrites, which would be a data race, not a saving")
		}
	})
}
