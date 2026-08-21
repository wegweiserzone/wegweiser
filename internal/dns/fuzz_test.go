package dns

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	wire "github.com/miekg/dns"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// A malformed packet that panics is a remote denial of service, so this is the
// one place in the codebase where "never panic" is a requirement rather than a
// goal. Both targets below therefore assert properties, not outputs: whatever
// comes back has to be a message a client can read, has to name the query it
// answers, and has to fit what the transport can carry.

// budgetFor is the largest response the fuzz targets accept for a query, worked
// out the same way the responder works it out. It mirrors production code
// deliberately: the property being defended is that no input makes a response
// exceed the ceiling, and an independent limit here would only test itself.
func budgetFor(query []byte, tr Transport) int {
	if tr == TCP {
		return wire.MaxMsgSize
	}

	m := new(wire.Msg)
	if err := m.Unpack(query); err != nil && m.IsEdns0() == nil {
		// It did not parse far enough to reach an OPT, so the unextended
		// datagram limit of RFC 1035 §4.2.1 applies. A query that only half
		// parses can still carry one, which is why the error alone does not
		// decide: the mirror has to look where the responder looks.
		return wire.MinMsgSize
	}

	opt := m.IsEdns0()
	if opt == nil {
		return wire.MinMsgSize
	}
	return min(max(int(opt.UDPSize()), wire.MinMsgSize), safeUDPResponse)
}

// checkResponse asserts everything that must hold of any response to any query.
func checkResponse(t *testing.T, query, packed []byte, tr Transport) {
	t.Helper()

	if len(packed) > budgetFor(query, tr) {
		t.Fatalf("the response is %d octets, above the %d this transport carries",
			len(packed), budgetFor(query, tr))
	}

	got := new(wire.Msg)
	if err := got.Unpack(packed); err != nil {
		t.Fatalf("the response this server built does not parse: %v", err)
	}
	if !got.Response {
		t.Fatal("the response does not have QR set, so a client would read it as a query")
	}
	if id := binary.BigEndian.Uint16(query[0:2]); got.Id != id {
		t.Fatalf("the response carries ID %#04x, want the query's %#04x", got.Id, id)
	}
	if opcode := int(query[2]>>3) & 0xF; got.Opcode != opcode {
		t.Fatalf("the response carries opcode %d, want the query's %d", got.Opcode, opcode)
	}
	if len(got.Question) > 1 {
		t.Fatalf("the response echoes %d questions, and a response never carries more than one",
			len(got.Question))
	}
	for _, rr := range got.Answer {
		if rr.Header().Name == "" {
			t.Fatal("a record in the answer has no owner name")
		}
	}
}

// qname returns the question name of a message as the octets it occupies on the
// wire. The question is the first thing after the header in both a query and a
// response, so the name there is never compressed.
func qname(t *testing.T, msg []byte) []byte {
	t.Helper()

	for off := headerLen; off < len(msg); {
		n := int(msg[off])
		switch {
		case n == 0:
			return msg[headerLen : off+1]
		case n&0xC0 != 0:
			t.Fatalf("the question name at offset %d is not a plain label", off)
		}
		off += 1 + n
	}
	t.Fatal("the question name is not terminated")
	return nil
}

// FuzzRespond feeds arbitrary bytes to the message layer. Most of them are not
// queries at all, which is the point: everything reachable from a socket has to
// end in either a readable response or a deliberate drop.
func FuzzRespond(f *testing.F) {
	snap := resolveFixture(f)

	seeds := [][]byte{
		nil,
		{},
		make([]byte, headerLen),
		make([]byte, headerLen-1),
	}
	for _, s := range []struct {
		name  string
		qtype zone.RRType
		edns  bool
	}{
		{"www.example.com.", zone.TypeA, false},
		{"www.example.com.", zone.TypeA, true},
		{"WwW.ExAmPlE.cOm.", zone.TypeAAAA, true},
		{"nothere.example.com.", zone.TypeA, false},
		{"sub2.example.com.", zone.TypeNS, true},
		{"alias.example.com.", zone.TypeA, false},
		{"loop1.example.com.", zone.TypeA, true},
		{"x.wild.example.com.", zone.TypeA, false},
		{"example.com.", zone.TypeANY, true},
		{"www.example.org.", zone.TypeA, false},
		{".", zone.TypeSOA, false},
		// The packer writes an empty name as no name at all, which produces a
		// question the unpacker cannot read back. It is not a query any client
		// sends, but it is a packet this server can be handed.
		{"", zone.TypeA, false},
	} {
		m := new(wire.Msg)
		m.SetQuestion(s.name, uint16(s.qtype))
		if s.edns {
			m.SetEdns0(4096, false)
		}
		if b, err := m.Pack(); err == nil {
			seeds = append(seeds, b)
		}
	}
	for _, s := range seeds {
		f.Add(s, false)
		f.Add(s, true)
	}

	f.Fuzz(func(t *testing.T, query []byte, overTCP bool) {
		tr := UDP
		if overTCP {
			tr = TCP
		}

		// A responder per call rather than one for the whole target: it holds
		// scratch and is documented as belonging to a single goroutine, so
		// sharing one here would test something other than the code.
		r := NewResponder(DefaultLimits())
		packed, err := r.Respond(snap, query, tr, make([]byte, wire.MaxMsgSize))

		if err != nil {
			if packed != nil {
				t.Fatalf("%d octets came back alongside the error %v", len(packed), err)
			}
			if !errors.Is(err, ErrUnanswerable) {
				t.Fatalf("a query failed for a reason other than being unanswerable: %v", err)
			}
			return
		}
		checkResponse(t, query, packed, tr)
	})
}

// FuzzRespondQuestion fuzzes the question rather than the packet.
func FuzzRespondQuestion(f *testing.F) {
	snap := resolveFixture(f)

	for _, s := range []struct {
		name   string
		qtype  uint16
		qclass uint16
	}{
		{"www.example.com.", uint16(zone.TypeA), wire.ClassINET},
		{"WWW.EXAMPLE.COM.", uint16(zone.TypeA), wire.ClassINET},
		{"wWw.eXaMpLe.CoM.", uint16(zone.TypeANY), wire.ClassINET},
		{"x.wild.example.com.", uint16(zone.TypeA), wire.ClassINET},
		{"sub2.example.com.", uint16(zone.TypeNS), wire.ClassINET},
		{"alias.example.com.", uint16(zone.TypeA), wire.ClassINET},
		{"loop1.example.com.", uint16(zone.TypeA), wire.ClassINET},
		{"example.com.", uint16(zone.TypeAXFR), wire.ClassINET},
		{"example.com.", uint16(zone.TypeSOA), wire.ClassCHAOS},
		{`a\.b.example.com.`, uint16(zone.TypeA), wire.ClassINET},
		{`\065.example.com.`, uint16(zone.TypeA), wire.ClassINET},
		{".", uint16(zone.TypeSOA), wire.ClassINET},
		{"a.b.c.d.e.f.g.example.com.", uint16(zone.TypeTXT), wire.ClassINET},
	} {
		f.Add(s.name, s.qtype, s.qclass, false)
	}

	f.Fuzz(func(t *testing.T, name string, qtype, qclass uint16, edns bool) {
		m := new(wire.Msg)
		m.SetQuestion(name, qtype)
		m.Question[0].Qclass = qclass
		if edns {
			m.SetEdns0(1232, false)
		}
		query, err := m.Pack()
		if err != nil {
			return // not a question that can be asked in the first place
		}
		// What was built and what went on the wire are not always the same
		// question: the packer writes a name it cannot read back, an empty one
		// becomes no name at all, and the bytes after it then parse as some
		// other question entirely. The server owes an echo of what it received,
		// so that is what this reads the expectation from. A query the library
		// cannot read back at all is malformed input and belongs in
		// FuzzRespond instead.
		sent := new(wire.Msg)
		if sent.Unpack(query) != nil || len(sent.Question) != 1 {
			return
		}

		r := NewResponder(DefaultLimits())
		packed, err := r.Respond(snap, query, UDP, make([]byte, wire.MaxMsgSize))
		if err != nil {
			t.Fatalf("a well-formed query was refused a response: %v", err)
		}
		checkResponse(t, query, packed, UDP)

		got := new(wire.Msg)
		if err := got.Unpack(packed); err != nil {
			t.Fatalf("the response does not parse: %v", err)
		}
		if len(got.Question) != 1 {
			t.Fatalf("the response echoes %d questions, want the one that was asked",
				len(got.Question))
		}
		// The comparison is on the wire bytes, not on the printed names. Two
		// spellings can encode to the same name, "\065" and "A" are the same
		// octet, and it is the octets a resolver checks its 0x20 nonce
		// against. Comparing what the library printed would fail on a
		// difference the wire does not have.
		if want, echoed := qname(t, query), qname(t, packed); !bytes.Equal(want, echoed) {
			t.Fatalf("the query name came back as % x, want % x exactly", echoed, want)
		}
		if q, w := got.Question[0], sent.Question[0]; q.Qtype != w.Qtype || q.Qclass != w.Qclass {
			t.Fatalf("the question came back as type %d class %d, want %d and %d",
				q.Qtype, q.Qclass, w.Qtype, w.Qclass)
		}

		switch got.Rcode {
		case wire.RcodeSuccess, wire.RcodeNameError, wire.RcodeRefused,
			wire.RcodeNotImplemented, wire.RcodeServerFailure:
		default:
			t.Fatalf("rcode = %s, which is not one this server decides to send",
				wire.RcodeToString[got.Rcode])
		}
		if got.Rcode == wire.RcodeNameError && !got.Authoritative {
			t.Fatal("a denial was sent without AA, so it asserts nothing")
		}
	})
}
