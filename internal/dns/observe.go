package dns

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	wire "github.com/miekg/dns"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// Event is one exchange, as everything watching the server sees it.
//
// It carries what the query path already knows by the time the response is on
// the wire, and nothing that would have to be looked up: an observer is on the
// reader's goroutine, and work done there is work not spent answering the next
// query (architecture §2.9).
type Event struct {
	// At is when the query was read, and Latency how long the exchange took
	// from that moment until the response had been written.
	At      time.Time
	Latency time.Duration

	// Client is where the query came from, and Transport how it arrived.
	Client    netip.AddrPort
	Transport Transport

	// Name, Type and Class are the question, as far as it could be read. Name
	// is empty when the message carried none; it is the name the client sent,
	// in the casing it sent, because that is what somebody watching the stream
	// is looking for.
	Name  string
	Type  zone.RRType
	Class zone.Class

	// Rcode is the response code sent, and Size the response in octets.
	Rcode int
	Size  int

	// Truncated is whether the response was cut to fit the transport and the
	// TC bit set (RFC 1035 §4.1.1).
	Truncated bool

	// Dropped is whether nothing was sent at all. Two messages have no safe
	// reply (architecture §2.2) and a response that cannot be packed
	// is a fault; either way the client waits for something that is not
	// coming, which is exactly what an operator is trying to find. Rcode and
	// Size mean nothing when it is set.
	Dropped bool
}

// String returns "udp" or "tcp", which is what a metric label and a stream
// entry both want to say.
func (t Transport) String() string {
	if t == TCP {
		return "tcp"
	}
	return "udp"
}

// RcodeName returns the mnemonic for a response code, or its number for one
// with no assigned name.
//
// Response codes are twelve bits and only a handful are assigned, so a caller
// grouping by them (a metric label, a summary line) wants the name where
// there is one and something stable where there is not.
func RcodeName(rcode int) string {
	if s, ok := wire.RcodeToString[rcode]; ok {
		return s
	}
	return "RCODE" + strconv.Itoa(rcode)
}

// ParseRcode is the inverse of [RcodeName]: it reads a response code written
// as a mnemonic, in any casing.
func ParseRcode(s string) (int, error) {
	if rcode, ok := wire.StringToRcode[strings.ToUpper(strings.TrimSpace(s))]; ok {
		return rcode, nil
	}
	return 0, fmt.Errorf("%q is not a response code", s)
}

// Observed describes the exchange the responder has just finished.
func (r *Responder) Observed() Event { return r.ev }

// observe hands a finished exchange to whoever is watching.
//
// It runs after the response has been written, so an observer that is slow
// delays the next query on this reader rather than this one's answer, and it
// is called on the reader's goroutine, so an observer that blocks stops a
// reader. Neither the metrics nor the ring buffer behind the query stream do:
// the buffer drops events rather than waiting, which is the trade §2.9 makes
// on purpose.
func (s *Server) observe(r *Responder, tr Transport, from netip.AddrPort, start time.Time, size int) {
	if s.cfg.Observe == nil {
		return
	}
	ev := r.Observed()
	ev.At = start
	ev.Latency = time.Since(start)
	ev.Client = from
	ev.Transport = tr
	ev.Size = size
	if size == 0 {
		// Nothing reached the wire: a query with no safe reply, a response
		// that would not pack, or a write that failed. A response is never
		// zero octets, a header alone is twelve, so the size says it.
		ev.Dropped = true
	}
	s.cfg.Observe(ev)
}

// startedAt is the moment an exchange began, or the zero time when nobody is
// watching and the clock would be read for nothing.
func (s *Server) startedAt() time.Time {
	if s.cfg.Observe == nil {
		return time.Time{}
	}
	return time.Now()
}
