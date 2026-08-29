package dns

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"slices"
	"time"

	wire "github.com/miekg/dns"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// transferBudget is how many octets of records one transfer message carries
// before the next one starts. A length prefix is two octets (RFC 1035 §4.2.2)
// so 65535 is the ceiling; the margin covers the header, the question, and
// that the estimate is of uncompressed records while the wire form compresses.
const transferBudget = 60000

// Transfers decides who may pull a whole zone off this server.
//
// A nil one allows nobody, which is what an unconfigured server does: a zone
// is an inventory of a network, and a copy handed out cannot be taken back
// (docs/decisions/d26-outbound-zone-transfer.md).
type Transfers interface {
	// MayTransfer decides one request. key is the name the request signed with
	// and verified against, or the zero name when it carried no signature.
	MayTransfer(client netip.Addr, key, apex zone.Name) bool
}

// Allow permits a transfer to a client inside one of the prefixes, or to one
// holding one of the keys. Both empty permits nobody, and nothing here spells
// everybody by accident.
type Allow struct {
	Prefixes []netip.Prefix
	Keys     []zone.Name
}

// MayTransfer implements [Transfers]. One entry matching is enough, which is
// what makes a key an entry rather than a second condition
// (docs/decisions/d28-tsig.md).
func (a Allow) MayTransfer(client netip.Addr, key, _ zone.Name) bool {
	if !key.IsZero() {
		for _, k := range a.Keys {
			if k.Equal(key) {
				return true
			}
		}
	}
	// A client reaching a dual-stack socket over IPv4 arrives as ::ffff:a.b.c.d,
	// which no IPv4 prefix contains.
	client = client.Unmap()
	for _, p := range a.Prefixes {
		if p.Contains(client) {
			return true
		}
	}
	return false
}

// transferQuery returns the zone a transfer query names and which of the two
// kinds it asks for.
//
// This only routes: the transfer path unpacks the query properly. Reading the
// question twice costs a name parse on a request that arrives rarely, and buys
// leaving the path every other query takes exactly as it was.
func transferQuery(query []byte) (zone.Name, zone.RRType, bool) {
	const (
		qr     = 1 << 15
		opcode = 0x7800
	)
	if len(query) < headerLen {
		return zone.Name{}, 0, false
	}
	flags := binary.BigEndian.Uint16(query[2:4])
	if flags&qr != 0 || flags&opcode != 0 {
		return zone.Name{}, 0, false
	}
	if binary.BigEndian.Uint16(query[4:6]) != 1 {
		return zone.Name{}, 0, false
	}

	name, off, err := wire.UnpackDomainName(query, headerLen)
	if err != nil || len(query) < off+4 {
		return zone.Name{}, 0, false
	}
	typ := zone.RRType(binary.BigEndian.Uint16(query[off : off+2]))
	if typ != zone.TypeAXFR && typ != zone.TypeIXFR {
		return zone.Name{}, 0, false
	}
	if binary.BigEndian.Uint16(query[off+2:off+4]) != uint16(zone.ClassIN) {
		return zone.Name{}, 0, false
	}

	apex, err := zone.ParseName(name)
	if err != nil {
		return zone.Name{}, 0, false
	}
	return apex, typ, true
}

// transferWriter fills transfer messages and sends each one once it is full.
//
// A transfer is one answer spread over as many messages as it takes, and where
// the break falls is the server's choice (RFC 5936 §2.2). Both kinds of
// transfer fill to the same budget through this.
type transferWriter struct {
	msg  *wire.Msg
	send func(*wire.Msg) error
	used int
}

// add appends a record, sending what has accumulated first if it would not fit.
func (w *transferWriter) add(rr wire.RR) error {
	n := wire.Len(rr)
	if w.used+n > transferBudget && len(w.msg.Answer) > 0 {
		if err := w.flush(); err != nil {
			return err
		}
	}
	w.msg.Answer = append(w.msg.Answer, rr)
	w.used += n
	return nil
}

// flush sends what has accumulated and starts the next message.
func (w *transferWriter) flush() error {
	if err := w.send(w.msg); err != nil {
		return err
	}
	w.msg.Answer, w.used = w.msg.Answer[:0], 0
	return nil
}

// apexSOA is the zone's start of authority as it is answered. It frames every
// transfer, whole (RFC 5936 §2.2) or incremental (RFC 1995 §4).
func (t *zoneTree) apexSOA() (wire.RR, error) {
	set := t.nodes[t.name].find(zone.ClassIN, zone.TypeSOA)
	if set == nil || len(set.rrs) != 1 {
		return nil, fmt.Errorf("the zone %s has no single start of authority to transfer", t.name)
	}
	return set.rrs[0], nil
}

// axfr streams a full transfer of one zone as RFC 5936 §2.2 describes it: the
// start of authority, every record, and the start of authority again.
func (t *zoneTree) axfr(w *transferWriter) error {
	soa, err := t.apexSOA()
	if err != nil {
		return err
	}

	// Sorted, so that transferring one zone twice sends the same thing twice.
	// RFC 5936 asks for no order beyond the start of authority first and last.
	names := make([]zone.Name, 0, len(t.nodes))
	for n := range t.nodes {
		names = append(names, n)
	}
	slices.SortFunc(names, func(a, b zone.Name) int { return a.Compare(b) })

	if aerr := w.add(soa); aerr != nil {
		return aerr
	}
	for _, name := range names {
		n := t.nodes[name]
		for i := range n.sets {
			st := &n.sets[i]
			// The start of authority leads and closes the transfer, so it does
			// not appear in the middle as well.
			if st.typ == zone.TypeSOA && name.Equal(t.name) {
				continue
			}
			for _, rr := range st.rrs {
				if aerr := w.add(rr); aerr != nil {
					return aerr
				}
			}
		}
	}
	if aerr := w.add(soa); aerr != nil {
		return aerr
	}
	return w.flush()
}

// transfer answers a request for a zone on conn, whole or incremental.
//
// A transfer is one question and many messages, which is why it is here rather
// than in [Responder.Respond]: that answers one with one.
func (s *Server) transfer(
	conn *net.TCPConn, query []byte, from netip.AddrPort,
	apex zone.Name, qtype zone.RRType, start time.Time,
) error {
	var req wire.Msg
	if err := req.Unpack(query); err != nil {
		return err
	}

	msg := &wire.Msg{
		MsgHdr: wire.MsgHdr{
			Id: req.Id, Response: true, Opcode: wire.OpcodeQuery, Authoritative: true,
		},
		Question: req.Question,
	}

	// One event for the whole transfer rather than one per message: to the
	// client this is a single exchange, so the latency says how long its zone
	// took to arrive and the size says how much of it did.
	sent := 0
	defer func() {
		ev := Event{
			At: start, Latency: time.Since(start), Client: from, Transport: TCP,
			Type: qtype, Class: zone.ClassIN,
			Rcode: msg.Rcode, Size: sent, Dropped: sent == 0,
		}
		if len(req.Question) == 1 {
			ev.Name = req.Question[0].Name
		}
		s.emit(ev)
	}()
	var (
		frame, packed []byte
		signer        *tsigSigner
	)
	// write frames one already-packed message and puts it on the connection.
	// RFC 1035 §4.2.2 prefixes each with its length.
	write := func(out []byte) error {
		// A record on its own larger than the budget is packed anyway, because
		// a message has to carry it or it can never be sent at all. One larger
		// than a length prefix can frame can never be sent either way, and
		// saying so beats sending a length that means something else.
		size := len(out)
		if size > math.MaxUint16 {
			return fmt.Errorf("a transfer message of %d octets cannot be framed", size)
		}

		frame = growTo(frame, size+2)
		binary.BigEndian.PutUint16(frame, uint16(size))
		copy(frame[2:], out)

		if derr := conn.SetWriteDeadline(time.Now().Add(s.cfg.TCPIdleTimeout)); derr != nil {
			return derr
		}
		if _, werr := conn.Write(frame); werr != nil {
			return werr
		}
		sent += len(frame)
		return nil
	}

	send := func(m *wire.Msg) error {
		var err error
		if signer != nil {
			// Every message of a transfer is signed. RFC 8945 §5.3.1 permits
			// ninety-nine unsigned in a row for the benefit of old clients; it
			// is a concession, not a budget (docs/decisions/d28-tsig.md).
			if packed, err = signer.sign(m); err != nil {
				return err
			}
		} else if packed, err = m.PackBuffer(packed[:0]); err != nil {
			return fmt.Errorf("pack a transfer message: %w", err)
		}
		return write(packed)
	}

	fail := func(rcode int, code uint16, text string) error {
		msg.Rcode = rcode
		msg.Authoritative = false
		msg.Answer = nil
		if opt := req.IsEdns0(); opt != nil {
			out := new(wire.OPT)
			out.Hdr = wire.RR_Header{Name: ".", Rrtype: wire.TypeOPT}
			out.SetUDPSize(s.cfg.Limits.MaxUDPResponse)
			out.Option = []wire.EDNS0{&wire.EDNS0_EDE{InfoCode: code, ExtraText: text}}
			msg.Extra = []wire.RR{out}
		}
		return send(msg)
	}

	// A signature is checked before anything is decided on it. An unsigned
	// request is not a failure here; whether it is enough is the list's
	// question (docs/decisions/d28-tsig.md).
	signed, verr := verifyTSIG(s.keyring(), query, &req)
	var refusal *tsigError
	switch {
	case errors.As(verr, &refusal):
		msg.Rcode = wire.RcodeNotAuth
		msg.Authoritative = false
		out, perr := signRefusal(msg, refusal.rr, refusal.key, refusal.code, time.Now())
		if perr != nil {
			return perr
		}
		return write(out)
	case verr != nil:
		return verr
	}
	signer = signed.signer

	if !s.mayTransfer(from.Addr(), signed.key, apex) {
		return fail(wire.RcodeRefused, wire.ExtendedErrorCodeProhibited,
			"this server does not transfer zones to you")
	}
	// The snapshot and nothing else, so a full transfer stays inside invariant
	// 2. An incremental one reads the journal as well, through [History].
	t := s.current.Load().zoneAt(apex)
	if t == nil {
		return fail(wire.RcodeRefused, wire.ExtendedErrorCodeNotAuthoritative,
			"no zone on this server has that name")
	}

	// Claimed here rather than when the connection arrived: an unauthorised
	// request must not be able to spend the budget, and everything above this
	// point is cheap. SERVFAIL rather than REFUSED, because the answer is
	// "not now" and not "not you": a secondary retries on its SOA timer, and
	// REFUSED would tell it the arrangement is over.
	if !s.takeTransfer() {
		return fail(wire.RcodeServerFailure, wire.ExtendedErrorCodeNotReady,
			"this server is already sending as many zone transfers as it will at once")
	}
	defer s.releaseTransfer()

	w := &transferWriter{msg: msg, send: send}
	if qtype == zone.TypeIXFR {
		since, ok := clientSerial(&req)
		if !ok {
			return fail(wire.RcodeFormatError, wire.ExtendedErrorCodeOther,
				"an incremental transfer says which version it holds, "+
					"in one start of authority in the authority section (RFC 1995 §3)")
		}
		return s.ixfr(t, since, w)
	}
	// Where this fails, half a zone has already gone down the connection and
	// there is no taking it back. Hanging up is what tells the client not to
	// use what it got.
	return t.axfr(w)
}

// transferHolder wraps the policy so it can be swapped atomically. An
// interface cannot be the element of an [atomic.Pointer] on its own.
type transferHolder struct{ policy Transfers }

// SetTransfers publishes who may pull a whole zone, for every transfer from
// here on. A nil one allows nobody.
func (s *Server) SetTransfers(t Transfers) {
	s.transferring.Store(&transferHolder{policy: t})
}

// mayTransfer answers for the server, so that a policy nobody configured is a
// refusal rather than a panic.
func (s *Server) mayTransfer(client netip.Addr, key, apex zone.Name) bool {
	h := s.transferring.Load()
	if h == nil || h.policy == nil {
		return false
	}
	return h.policy.MayTransfer(client, key, apex)
}
