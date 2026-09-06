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

	// Refused says the query was turned away for want of a cookie while the
	// server was under load (D35), and what the client had brought with it.
	// [NotRefused] on every other exchange, which is nearly all of them.
	Refused Refusal

	// Dropped is whether nothing was sent at all. Two messages have no safe
	// reply (architecture §2.2) and a response that cannot be packed
	// is a fault; either way the client waits for something that is not
	// coming, which is exactly what an operator is trying to find. Rcode and
	// Size mean nothing when it is set.
	Dropped bool
}

// Refusal says why an exchange was refused for want of a cookie, and tells
// apart the two clients D35 treats differently: the one that can come back
// with the cookie it was just handed, and the one that implements no cookies
// at all and cannot.
//
// The second is what D35 names as the case its decision leaves exposed, and
// counting it is where the measurement it asks for before response rate
// limiting is reopened would come from.
type Refusal uint8

const (
	// NotRefused is every exchange this did not happen to.
	NotRefused Refusal = iota
	// RefusedCookieless is a query that carried no cookie option at all.
	// There is nothing to hand such a client back, so the refusal is a plain
	// REFUSED.
	RefusedCookieless
	// RefusedBadCookie is a query whose Server Cookie was not one of ours, or
	// had expired. It is answered BADCOOKIE with a fresh cookie in it, so one
	// retry is the whole cost.
	RefusedBadCookie
)

// String names the refusal for a metric label, and is empty for an exchange
// that was not refused.
func (r Refusal) String() string {
	switch r {
	case RefusedCookieless:
		return "cookieless"
	case RefusedBadCookie:
		return "badcookie"
	default:
		return ""
	}
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
	s.emit(ev)
}

// emit hands one event to whoever is watching. A transfer builds its own
// rather than taking it from a [Responder], and both end here.
func (s *Server) emit(ev Event) {
	if s.cfg.Observe == nil {
		return
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
