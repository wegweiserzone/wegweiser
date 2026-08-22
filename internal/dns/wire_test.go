package dns

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	wire "github.com/miekg/dns"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// queryID is the identifier every query in these tests carries, so that a
// response can be checked for echoing it rather than for holding some number.
const queryID = 0x2A2A

// packQuery builds the bytes of a query the way a client would.
func packQuery(t *testing.T, name string, qtype zone.RRType, shape ...func(*wire.Msg)) []byte {
	t.Helper()

	m := new(wire.Msg)
	m.SetQuestion(name, uint16(qtype))
	m.Id = queryID
	for _, s := range shape {
		s(m)
	}

	b, err := m.Pack()
	if err != nil {
		t.Fatalf("pack the query: %v", err)
	}
	return b
}

// withEDNS gives the query an OPT record advertising size, at an EDNS version
// that is usually but not always the only one defined.
func withEDNS(size uint16, version uint8) func(*wire.Msg) {
	return func(m *wire.Msg) {
		opt := &wire.OPT{Hdr: wire.RR_Header{Name: ".", Rrtype: wire.TypeOPT}}
		opt.SetUDPSize(size)
		opt.SetVersion(version)
		m.Extra = append(m.Extra, opt)
	}
}

// respond runs one exchange and returns the response, already unpacked.
func respond(t *testing.T, r *Responder, snap *Snapshot, query []byte, tr Transport,
) (msg *wire.Msg, packed []byte) {
	t.Helper()

	packed, err := r.Respond(snap, query, tr, make([]byte, wire.MaxMsgSize))
	if err != nil {
		t.Fatalf("respond: %v", err)
	}

	got := new(wire.Msg)
	if err := got.Unpack(packed); err != nil {
		t.Fatalf("the response does not parse: %v", err)
	}
	if !got.Response {
		t.Error("the response does not have QR set")
	}
	if got.Id != queryID {
		t.Errorf("the response echoes ID %#04x, want %#04x", got.Id, queryID)
	}
	return got, packed
}

func TestRespond(t *testing.T) {
	t.Parallel()

	snap := resolveFixture(t)
	r := NewResponder(DefaultLimits())

	got, _ := respond(t, r, snap, packQuery(t, "www.example.com.", zone.TypeA), UDP)

	if got.Rcode != wire.RcodeSuccess {
		t.Errorf("rcode = %s, want NOERROR", wire.RcodeToString[got.Rcode])
	}
	if !got.Authoritative {
		t.Error("AA is not set on an answer from a zone we hold")
	}
	if got.RecursionAvailable {
		t.Error("RA is set, and this server does not recurse")
	}
	if !got.RecursionDesired {
		t.Error("RD was set in the query and is not echoed (RFC 1035 §4.1.1)")
	}
	if got.Truncated {
		t.Error("TC is set on a response that fits")
	}
	want := []string{
		"www.example.com. 300 IN A 192.0.2.10",
		"www.example.com. 300 IN A 192.0.2.11",
	}
	if answer := lines(got.Answer); !slices.Equal(answer, want) {
		t.Errorf("answer section:\n got %q\nwant %q", answer, want)
	}
	if len(got.Question) != 1 || got.Question[0].Name != "www.example.com." {
		t.Errorf("question section = %+v, want the query's own", got.Question)
	}
	if got.IsEdns0() != nil {
		t.Error("the response carries an OPT although the query did not")
	}
}

// TestRespondEchoesCase covers 0x20 encoding: a resolver that randomises the
// case of its query names checks the response against what it sent, and a
// server that answers in its own stored case fails that check.
func TestRespondEchoesCase(t *testing.T) {
	t.Parallel()

	snap := resolveFixture(t)

	for _, tt := range []struct {
		name  string
		qname string
		owner string
	}{
		{"mixed case is echoed everywhere it was asked about",
			"WwW.ExAmPlE.cOm.", "WwW.ExAmPlE.cOm."},
		{"lowercase is left exactly as it is stored",
			"www.example.com.", "www.example.com."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := NewResponder(DefaultLimits())
			got, _ := respond(t, r, snap, packQuery(t, tt.qname, zone.TypeA), UDP)

			if got.Question[0].Name != tt.qname {
				t.Errorf("question echoes %q, want %q", got.Question[0].Name, tt.qname)
			}
			for _, rr := range got.Answer {
				if rr.Header().Name != tt.owner {
					t.Errorf("answer owner %q, want %q", rr.Header().Name, tt.owner)
				}
			}
		})
	}
}

// TestRespondEchoesCaseThroughCNAME checks that only the name the client asked
// about is rewritten. The rest of a chain is data it did not name, and changing
// its case would be inventing a difference.
func TestRespondEchoesCaseThroughCNAME(t *testing.T) {
	t.Parallel()

	snap := resolveFixture(t)
	r := NewResponder(DefaultLimits())

	got, _ := respond(t, r, snap, packQuery(t, "ALIAS.example.com.", zone.TypeA), UDP)

	want := []string{
		"ALIAS.example.com. 300 IN CNAME www.example.com.",
		"www.example.com. 300 IN A 192.0.2.10",
		"www.example.com. 300 IN A 192.0.2.11",
	}
	if answer := lines(got.Answer); !slices.Equal(answer, want) {
		t.Errorf("answer section:\n got %q\nwant %q", answer, want)
	}
}

// TestRespondDrops covers the two queries that get no response at all. Both are
// silence on purpose: there is nothing truthful to say, and saying something
// anyway is how a server becomes an amplifier or one half of a loop.
func TestRespondDrops(t *testing.T) {
	t.Parallel()

	snap := resolveFixture(t)

	valid := packQuery(t, "www.example.com.", zone.TypeA)
	response := slices.Clone(valid)
	binary.BigEndian.PutUint16(response[2:4], binary.BigEndian.Uint16(response[2:4])|0x8000)

	for _, tt := range []struct {
		name  string
		query []byte
	}{
		{"shorter than a header", valid[:headerLen-1]},
		{"empty", nil},
		{"already a response", response},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := NewResponder(DefaultLimits())
			out, err := r.Respond(snap, tt.query, UDP, make([]byte, wire.MaxMsgSize))
			if !errors.Is(err, ErrUnanswerable) {
				t.Errorf("error = %v, want ErrUnanswerable", err)
			}
			if out != nil {
				t.Errorf("%d bytes were returned for a query that must be dropped", len(out))
			}
		})
	}
}

// TestRespondRejects walks the table of architecture §2.2. Every one of
// these gets an answer rather than silence: a client that receives nothing
// cannot tell a query it should fix from a server it should stop asking.
func TestRespondRejects(t *testing.T) {
	t.Parallel()

	snap := resolveFixture(t)

	// A query promising an answer record, followed by a byte that starts a
	// compression pointer and then ends. Nothing after the question parses.
	brokenBody := packQuery(t, "www.example.com.", zone.TypeA)
	brokenBody = append(brokenBody, 0xC0)
	binary.BigEndian.PutUint16(brokenBody[6:8], 1)

	// A query cut off inside its question, so not even that survives.
	brokenQuestion := packQuery(t, "www.example.com.", zone.TypeA)
	brokenQuestion = brokenQuestion[:len(brokenQuestion)-3]

	for _, tt := range []struct {
		name      string
		query     []byte
		rcode     int
		questions int

		// wantEDE is false where the query carries no OPT for one to ride in.
		// A message whose body does not parse is always in that position: the
		// OPT sits in the last section, and unpacking stops at the first
		// section that fails, so it is never recovered.
		wantEDE bool
		edeCode uint16
	}{
		{
			name:  "a body that does not parse, with the question still readable",
			query: brokenBody, rcode: wire.RcodeFormatError, questions: 1,
		},
		{
			name:  "a question that does not parse either",
			query: brokenQuestion, rcode: wire.RcodeFormatError, questions: 0,
		},
		{
			name: "an opcode this server does not implement",
			query: packQuery(t, "example.com.", zone.TypeSOA, withEDNS(1232, 0),
				func(m *wire.Msg) { m.Opcode = wire.OpcodeUpdate }),
			rcode: wire.RcodeNotImplemented, questions: 1,
			wantEDE: true, edeCode: wire.ExtendedErrorCodeNotSupported,
		},
		{
			name: "no question at all",
			query: packQuery(t, "example.com.", zone.TypeSOA, withEDNS(1232, 0),
				func(m *wire.Msg) { m.Question = nil }),
			rcode: wire.RcodeFormatError, questions: 0,
			wantEDE: true, edeCode: wire.ExtendedErrorCodeOther,
		},
		{
			name: "two questions, which no single RCODE can answer",
			query: packQuery(t, "example.com.", zone.TypeSOA, withEDNS(1232, 0),
				func(m *wire.Msg) {
					m.Question = append(m.Question, wire.Question{
						Name: "www.example.com.", Qtype: wire.TypeA, Qclass: wire.ClassINET,
					})
				}),
			rcode: wire.RcodeFormatError, questions: 0,
			wantEDE: true, edeCode: wire.ExtendedErrorCodeOther,
		},
		{
			name:  "an EDNS version from the future",
			query: packQuery(t, "www.example.com.", zone.TypeA, withEDNS(1232, 1)),
			rcode: wire.RcodeBadVers, questions: 1,
			wantEDE: true, edeCode: wire.ExtendedErrorCodeOther,
		},
		{
			name: "a class this server holds nothing in",
			query: packQuery(t, "version.bind.", zone.TypeTXT, withEDNS(1232, 0),
				func(m *wire.Msg) { m.Question[0].Qclass = wire.ClassCHAOS }),
			rcode: wire.RcodeRefused, questions: 1,
			wantEDE: true, edeCode: wire.ExtendedErrorCodeNotSupported,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := NewResponder(DefaultLimits())
			got, _ := respond(t, r, snap, tt.query, UDP)

			if got.Rcode != tt.rcode {
				t.Errorf("rcode = %s, want %s",
					wire.RcodeToString[got.Rcode], wire.RcodeToString[tt.rcode])
			}
			if len(got.Question) != tt.questions {
				t.Errorf("the response echoes %d questions, want %d",
					len(got.Question), tt.questions)
			}
			if len(got.Answer) != 0 {
				t.Errorf("a rejected query got %d answer records", len(got.Answer))
			}

			opt := got.IsEdns0()
			if !tt.wantEDE {
				if opt != nil {
					t.Error("the response carries an OPT although none was recovered " +
						"from the query")
				}
				return
			}
			if opt == nil {
				t.Fatal("the response carries no OPT, so it carries no extended error")
			}
			if opt.Version() != 0 {
				t.Errorf("the response advertises EDNS version %d, want 0", opt.Version())
			}
			ede := extendedError(opt)
			if ede == nil {
				t.Fatal("the response carries no extended error to explain itself")
			}
			if ede.InfoCode != tt.edeCode {
				t.Errorf("extended error = %d (%s), want %d (%s)",
					ede.InfoCode, wire.ExtendedErrorCodeToString[ede.InfoCode],
					tt.edeCode, wire.ExtendedErrorCodeToString[tt.edeCode])
			}
			if ede.ExtraText == "" {
				t.Error("the extended error carries no text, which is the point of having one")
			}
		})
	}
}

// extendedError returns the RFC 8914 option in an OPT record, or nil.
func extendedError(opt *wire.OPT) *wire.EDNS0_EDE {
	for _, o := range opt.Option {
		if ede, ok := o.(*wire.EDNS0_EDE); ok {
			return ede
		}
	}
	return nil
}

// TestRespondEDNS covers what an OPT in the query obliges the response to say.
func TestRespondEDNS(t *testing.T) {
	t.Parallel()

	snap := resolveFixture(t)
	limits := Limits{MaxUDPResponse: 1232}

	t.Run("a query with an OPT gets one back, sized by this server", func(t *testing.T) {
		t.Parallel()

		r := NewResponder(limits)
		got, _ := respond(t, r, snap,
			packQuery(t, "www.example.com.", zone.TypeA, withEDNS(4096, 0)), UDP)

		opt := got.IsEdns0()
		if opt == nil {
			t.Fatal("the response carries no OPT")
		}
		if opt.UDPSize() != 1232 {
			t.Errorf("the response advertises %d octets, want this server's own 1232",
				opt.UDPSize())
		}
		if opt.Do() {
			t.Error("DO is set, and this server signs nothing")
		}
	})

	t.Run("a query without an OPT gets none back", func(t *testing.T) {
		t.Parallel()

		r := NewResponder(limits)
		got, _ := respond(t, r, snap, packQuery(t, "www.example.com.", zone.TypeA), UDP)
		if got.IsEdns0() != nil {
			t.Error("the response carries an OPT the query never asked for (RFC 6891 §6.1.1)")
		}
	})

	t.Run("an answer without an OPT carries no extended error", func(t *testing.T) {
		t.Parallel()

		// REFUSED would carry one if there were anywhere to put it.
		r := NewResponder(limits)
		got, _ := respond(t, r, snap, packQuery(t, "www.example.org.", zone.TypeA), UDP)
		if got.Rcode != wire.RcodeRefused {
			t.Errorf("rcode = %s, want REFUSED", wire.RcodeToString[got.Rcode])
		}
		if got.IsEdns0() != nil {
			t.Error("the response carries an OPT the query never asked for")
		}
	})
}

// bigZone is a snapshot holding one name with more addresses than any datagram
// can carry, which is the only way to reach the truncation rules.
func bigZone(t *testing.T) *Snapshot {
	t.Helper()

	records := make([]string, 0, 200)
	for i := range 200 {
		records = append(records,
			fmt.Sprintf("many.example.com. 300 IN A 192.0.%d.%d", i/256, i%256))
	}
	return snapshot(t, map[string][]string{"example.com.": records})
}

// TestRespondTruncates covers RFC 1035 §4.1.1: a datagram that cannot carry the
// whole answer carries as much as it can and says so, and the client comes back
// over TCP for the rest.
func TestRespondTruncates(t *testing.T) {
	t.Parallel()

	snap := bigZone(t)
	q := packQuery(t, "many.example.com.", zone.TypeA)
	qEDNS := packQuery(t, "many.example.com.", zone.TypeA, withEDNS(4096, 0))

	for _, tt := range []struct {
		name      string
		query     []byte
		tr        Transport
		limit     int
		truncated bool
	}{
		{"a datagram without EDNS holds 512 octets", q, UDP, 512, true},
		{"an advertised 4096 is still cut to this server's ceiling", qEDNS, UDP, 1232, true},
		{"a stream carries the whole answer", q, TCP, wire.MaxMsgSize, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := NewResponder(Limits{MaxUDPResponse: 1232})
			got, packed := respond(t, r, snap, tt.query, tt.tr)

			if len(packed) > tt.limit {
				t.Errorf("the response is %d octets, above the limit of %d",
					len(packed), tt.limit)
			}
			if got.Truncated != tt.truncated {
				t.Errorf("TC = %v, want %v", got.Truncated, tt.truncated)
			}
			if tt.truncated && len(got.Answer) == 0 {
				t.Error("a truncated response carries nothing at all, so a client " +
					"learns only that it should try again")
			}
			if !tt.truncated && len(got.Answer) != 200 {
				t.Errorf("the answer holds %d records, want all 200", len(got.Answer))
			}
		})
	}
}

// TestRespondReuse checks that a responder carries nothing from one exchange
// into the next, which is what makes reusing one per worker safe.
func TestRespondReuse(t *testing.T) {
	t.Parallel()

	snap := resolveFixture(t)
	r := NewResponder(DefaultLimits())

	// A refusal with an extended error and an OPT, then a plain answer without
	// either: everything the first response grew has to be gone.
	first, _ := respond(t, r, snap,
		packQuery(t, "www.example.org.", zone.TypeA, withEDNS(1232, 0)), UDP)
	if first.Rcode != wire.RcodeRefused || first.IsEdns0() == nil {
		t.Fatalf("the first response is not the refusal this test needs: %v", first.Rcode)
	}

	second, _ := respond(t, r, snap, packQuery(t, "www.example.com.", zone.TypeA), UDP)

	if second.Rcode != wire.RcodeSuccess || !second.Authoritative {
		t.Errorf("rcode = %s, AA = %v, want NOERROR with AA set",
			wire.RcodeToString[second.Rcode], second.Authoritative)
	}
	if second.IsEdns0() != nil {
		t.Error("the OPT of the previous exchange survived into a query without one")
	}
	if len(second.Answer) != 2 || len(second.Ns) != 0 {
		t.Errorf("sections hold %d answer and %d authority records, want 2 and 0",
			len(second.Answer), len(second.Ns))
	}
}

// TestRespondSmallBuffer checks that a buffer too small to pack into is grown
// rather than silently truncating the response.
func TestRespondSmallBuffer(t *testing.T) {
	t.Parallel()

	snap := resolveFixture(t)
	r := NewResponder(DefaultLimits())

	packed, err := r.Respond(snap, packQuery(t, "www.example.com.", zone.TypeA), UDP, nil)
	if err != nil {
		t.Fatalf("respond: %v", err)
	}
	got := new(wire.Msg)
	if err := got.Unpack(packed); err != nil {
		t.Fatalf("the response does not parse: %v", err)
	}
	if len(got.Answer) != 2 {
		t.Errorf("the answer holds %d records, want 2", len(got.Answer))
	}
}

// TestResponderLimits covers what a configured ceiling is worth: it clamps a
// requestor's claim from above, and RFC 6891 §6.2.3 clamps both of them from
// below at the 512 octets a datagram carries without EDNS at all.
func TestResponderLimits(t *testing.T) {
	t.Parallel()

	snap := bigZone(t)

	for _, tt := range []struct {
		name       string
		limits     Limits
		advertised uint16
		budget     int
	}{
		{"an unset ceiling takes the default", Limits{}, 4096, safeUDPResponse},
		{"a ceiling below 512 is raised to it", Limits{MaxUDPResponse: 100}, 4096, 512},
		{"a claim below 512 is raised to it", Limits{MaxUDPResponse: 4096}, 200, 512},
		{"a claim below the ceiling is honoured", Limits{MaxUDPResponse: 4096}, 1400, 1400},
		{"a claim above the ceiling is cut to it", Limits{MaxUDPResponse: 900}, 4096, 900},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := NewResponder(tt.limits)
			got, packed := respond(t, r, snap,
				packQuery(t, "many.example.com.", zone.TypeA, withEDNS(tt.advertised, 0)), UDP)

			if len(packed) > tt.budget {
				t.Errorf("the response is %d octets, above the budget of %d",
					len(packed), tt.budget)
			}
			if !got.Truncated {
				t.Error("TC is not set although the answer cannot fit any of these budgets")
			}
			// A budget one record short of the truth would still pass the check
			// above, so the response also has to be worth sending.
			if len(got.Answer) == 0 {
				t.Error("the response carries no records at all")
			}
		})
	}
}

// TestAmplificationFactor pins how much larger than a query a response can be.
//
// An authoritative server answering a spoofed source address is a reflector,
// and what makes reflection worth an attacker's trouble is that ratio. Every
// bound in this path exists to hold it down (the 1232-octet ceiling, the
// single RRset for ANY (RFC 8482), the sixteen additional records) and none of
// them is worth anything if nobody knows what they add up to.
//
// So the number is measured rather than assumed, and it is a test rather than a
// benchmark: raising MaxUDPResponse or loosening a bound is allowed, and doing
// it without noticing what it does to this ratio is not.
func TestAmplificationFactor(t *testing.T) {
	t.Parallel()

	// The largest answer a zone can be made to give: one RRset of TXT records
	// filling the response, at the shortest name that can hold it.
	lines := []string{
		"example.com. 3600 IN NS ns1.example.com.",
		"ns1.example.com. 3600 IN A 192.0.2.1",
	}
	for range 16 {
		lines = append(lines, `a.example.com. 3600 IN TXT "`+strings.Repeat("x", 255)+`"`)
	}
	snap := build(t, newZone(t, "example.com."), lines...)

	for _, tc := range []struct {
		name  string
		query []byte
		// worst is the ratio this case must stay under. They are the numbers
		// as measured, rounded up: a change that moves one is a change to how
		// useful this server is to somebody attacking a third party.
		worst float64
	}{
		{
			// No OPT, so RFC 1035 §4.2.1 caps the response at 512 octets
			// whatever the answer holds.
			name:  "without EDNS",
			query: packQuery(t, "a.example.com.", zone.TypeTXT),
			worst: 10, // measured 9.6
		},
		{
			name:  "with EDNS offering room",
			query: packQuery(t, "a.example.com.", zone.TypeTXT, withEDNS(4096, 0)),
			worst: 27, // measured 26.5, and the worst this server has
		},
		{
			// The classic lever: one small question, everything at the name.
			// RFC 8482 answers with one RRset instead.
			name:  "ANY",
			query: packQuery(t, "a.example.com.", zone.TypeANY, withEDNS(4096, 0)),
			worst: 27, // measured 26.5: no worse than asking for the type directly
		},
		{
			// A name we do not serve costs an attacker as much as it costs us.
			name:  "refused",
			query: packQuery(t, "a.example.net.", zone.TypeTXT, withEDNS(4096, 0)),
			worst: 3, // measured 2.1
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := NewResponder(DefaultLimits())
			_, got := respond(t, r, snap, tc.query, UDP)

			ratio := float64(len(got)) / float64(len(tc.query))
			if ratio > tc.worst {
				t.Errorf("%d octets answering %d is a factor of %.1f, over the %.0f this case "+
					"is allowed; something loosened a bound that holds reflection down",
					len(got), len(tc.query), ratio, tc.worst)
			}
			t.Logf("%d octets answering %d: factor %.1f", len(got), len(tc.query), ratio)
		})
	}
}

// TestRespondAllocations pins the allocation budget of docs/decisions.md D12.
//
// It does not call t.Parallel: AllocsPerRun measures the whole process, and a
// test running beside it would be counted here.
//
// Four is the number D12 settled on after measuring, not a target that happens
// to be met. Two are the wire library decoding the query name into presentation
// form and ParseName encoding it back, one is that string, one is the question
// slice; the note in wire.go says what removing three would cost. A change that
// moves this number is a change to that decision, so it fails here first.
func TestRespondAllocations(t *testing.T) {
	snap := resolveFixture(t)
	r := NewResponder(DefaultLimits())
	buf := make([]byte, wire.MaxMsgSize)

	for _, tt := range []struct {
		name  string
		qname string
		qtype zone.RRType
		want  float64
	}{
		{"hit", "www.example.com.", zone.TypeA, 4},
		{"a name no zone here covers", "www.example.org.", zone.TypeA, 4},
		{"a name that is not there", "nope.example.com.", zone.TypeA, 4},
	} {
		t.Run(tt.name, func(t *testing.T) {
			query := packQuery(t, tt.qname, tt.qtype)
			got := testing.AllocsPerRun(100, func() {
				if _, err := r.Respond(snap, query, UDP, buf); err != nil {
					t.Fatalf("respond: %v", err)
				}
			})
			if got != tt.want {
				t.Errorf("%.0f allocations per exchange, want %.0f (D12). "+
					"Fewer is good news and still needs D12 changing to match",
					got, tt.want)
			}
		})
	}
}
