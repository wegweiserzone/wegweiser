package dns

import (
	"errors"
	"fmt"
	"strings"

	wire "github.com/miekg/dns"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// headerLen is the fixed size of a message header, in octets (RFC 1035 §4.1.1).
// A message shorter than this carries no question and no ID to answer with.
const headerLen = 12

// safeUDPResponse is the largest UDP response this server sends unless it is
// configured otherwise.
const safeUDPResponse = 1232

// ErrUnanswerable reports a query that no response can be built for, so the
// only correct action is to drop it.
var ErrUnanswerable = errors.New("dns: the query cannot be answered and must be dropped")

// Transport is how a query reached the server.
//
// It decides how large a response may grow and whether truncation applies at
// all: RFC 1035 §4.2.1 caps an unextended datagram at 512 octets, while §4.2.2
// frames a stream message with a two-octet length instead.
type Transport uint8

const (
	// UDP is a datagram. A response too large for the requestor's buffer is
	// truncated and marked, so the client retries over TCP.
	UDP Transport = iota
	// TCP is a stream, where a response is not truncated for size.
	TCP
)

// Limits bound what the message layer will send.
type Limits struct {
	// MaxUDPResponse caps a UDP response however large a buffer the requestor
	// advertises. A requestor may claim to accept 4096 octets; whether the path
	// between us carries them is not something it can know.
	MaxUDPResponse uint16
}

// DefaultLimits returns the limits a server runs with unless an operator says
// otherwise.
func DefaultLimits() Limits { return Limits{MaxUDPResponse: safeUDPResponse} }

// Responder turns the bytes of a query into the bytes of a response.
//
// It holds the scratch one exchange needs (the parsed query, the response
// being assembled, the resolved answer) and reuses all of it, so a Responder
// belongs to a single worker goroutine and is **not** safe for concurrent use.
// A server keeps one per reader.
type Responder struct {
	limits Limits

	req  wire.Msg
	resp wire.Msg
	ans  Answer

	// opt is the OPT record of the response, and ede the extended error inside
	// it. Both are fields rather than values built per query, so that attaching
	// them costs no allocation.
	opt    wire.OPT
	ede    wire.EDNS0_EDE
	hasEDE bool

	// ev is what the last exchange looked like, for whoever is watching. It is
	// filled as the exchange goes rather than reconstructed afterwards,
	// because by then the responder has been handed the next query.
	ev Event
}

// NewResponder returns a responder bound to the given limits.
//
// A zero MaxUDPResponse means "unset" and takes the default. One below the 512
// octets of RFC 1035 §4.2.1 is raised to 512, because RFC 6891 §6.2.3 says a
// smaller advertised value is to be read as 512 and there is no reason for our
// own ceiling to behave differently.
func NewResponder(limits Limits) *Responder {
	switch {
	case limits.MaxUDPResponse == 0:
		limits.MaxUDPResponse = safeUDPResponse
	case limits.MaxUDPResponse < wire.MinMsgSize:
		limits.MaxUDPResponse = wire.MinMsgSize
	}
	return &Responder{limits: limits}
}

// Respond answers query from snap and returns the response as bytes.
//
// The response is packed into out when it fits; otherwise a new buffer is
// allocated, so a caller that wants neither should hand over a buffer of the
// largest message it is willing to send. The returned slice aliases out, and is
// only valid until the next call.
func (r *Responder) Respond(snap *Snapshot, query []byte, tr Transport, out []byte) ([]byte, error) {
	// Cleared first: the responder is reused, and an event left over from the
	// previous query would describe this one wrongly on every path that gives
	// up before filling it in.
	r.ev = Event{Dropped: true}

	if len(query) < headerLen {
		return nil, ErrUnanswerable
	}

	// A header this long always parses, so whatever else fails below, the ID
	// and the flags are readable and a response can name the query it answers.
	unpackErr := r.req.Unpack(query)
	if len(r.req.Question) == 1 {
		q := r.req.Question[0]
		r.ev.Name, r.ev.Type, r.ev.Class = q.Name, zone.RRType(q.Qtype), zone.Class(q.Qclass)
	}
	if r.req.Response {
		return nil, ErrUnanswerable
	}
	reqOPT := r.req.IsEdns0()

	r.begin()

	// The order is the table in architecture §2.2, and it is an order
	// rather than a set: a query can be wrong in several ways at once, and the
	// first answer is the one that tells the client the most about what to fix.
	switch {
	case unpackErr != nil:
		// RFC 1035 §4.1.1 has no code for "the rest of your message did not
		// parse". Whatever of the question survived is echoed by begin, so the
		// client can still tell which query went wrong.
		r.fail(wire.RcodeFormatError, wire.ExtendedErrorCodeOther,
			"the query is malformed past its header")

	case r.req.Opcode != wire.OpcodeQuery:
		// NOTIFY and UPDATE belong to a secondary and to dynamic update, and
		// v0.1 is neither (scope fence). Answering them would be a claim.
		r.fail(wire.RcodeNotImplemented, wire.ExtendedErrorCodeNotSupported,
			"this server implements the QUERY opcode only")

	case len(r.req.Question) != 1:
		r.fail(wire.RcodeFormatError, wire.ExtendedErrorCodeOther,
			"a query carries exactly one question")

	case reqOPT != nil && reqOPT.Version() != 0:
		// RFC 6891 §6.1.3: answer the version we do implement rather than
		// guessing at one we do not.
		r.fail(wire.RcodeBadVers, wire.ExtendedErrorCodeOther,
			"this server implements EDNS version 0")

	case zone.Class(r.req.Question[0].Qclass) != zone.ClassIN:
		// TODO: CH is where version.bind and friends live (RFC 4892). Until
		// they exist there is nothing to say in that class, and saying nothing
		// is what REFUSED means.
		r.fail(wire.RcodeRefused, wire.ExtendedErrorCodeNotSupported,
			"this server answers questions in class IN only")

	default:
		r.answer(snap)
	}

	return r.finish(reqOPT, tr, out)
}

// begin prepares the response for one exchange.
func (r *Responder) begin() {
	r.resp.Id = r.req.Id
	r.resp.Response = true
	r.resp.Opcode = r.req.Opcode
	r.resp.Rcode = wire.RcodeSuccess
	r.resp.Authoritative = false
	r.resp.Truncated = false
	r.resp.Zero = false
	r.resp.AuthenticatedData = false

	// RD is echoed because RFC 1035 §4.1.1 says it is copied into the response;
	// RA stays clear because this server does not recurse and never will.
	r.resp.RecursionDesired = r.req.RecursionDesired
	r.resp.RecursionAvailable = false
	r.resp.CheckingDisabled = r.req.CheckingDisabled

	r.resp.Question = r.resp.Question[:0]
	if len(r.req.Question) == 1 {
		r.resp.Question = append(r.resp.Question, r.req.Question[0])
	}
	r.resp.Answer = r.resp.Answer[:0]
	r.resp.Ns = r.resp.Ns[:0]
	r.resp.Extra = r.resp.Extra[:0]

	r.ede = wire.EDNS0_EDE{}
	r.hasEDE = false
}

// fail sets the response code and the extended error that explains it.
func (r *Responder) fail(rcode int, code uint16, text string) {
	r.resp.Rcode = rcode
	r.ede = wire.EDNS0_EDE{InfoCode: code, ExtraText: text}
	r.hasEDE = true
}

// answer resolves the question and copies the result into the response.
func (r *Responder) answer(snap *Snapshot) {
	q := r.req.Question[0]

	// Two of an exchange's four allocations are here: the wire library decodes
	// the name out of the packet into presentation form, and ParseName encodes
	// that string straight back to the wire form it came from. Reading the name
	// out of the query bytes with a zone.NameFromWire would leave one.
	//
	// Measured and left alone (docs/decisions.md D12). The kernel is about 95%
	// of what a query costs; these 128 octets are a fraction of the rest, and
	// buying them means a hand-written name parser in the path a malformed
	// packet reaches first. Revisit only if a benchmark on a real interface
	// shows allocation pressure mattering.
	name, err := zone.ParseName(q.Name)
	if err != nil {
		// The wire library accepted this name and we cannot read it, which
		// means the two disagree about what a name is. That is a bug on one
		// side or the other, and the safe reply is the one that does not
		// pretend to have looked the name up.
		r.fail(wire.RcodeFormatError, wire.ExtendedErrorCodeOther,
			"the query name is not one this server can read")
		return
	}

	snap.Resolve(Question{
		Name:  name,
		Class: zone.Class(q.Qclass),
		Type:  zone.RRType(q.Qtype),
	}, &r.ans)

	r.resp.Rcode = r.ans.Rcode
	r.resp.Authoritative = r.ans.Authoritative
	r.resp.Answer = append(r.resp.Answer, r.ans.Answer...)
	r.resp.Ns = append(r.resp.Ns, r.ans.Authority...)
	r.resp.Extra = append(r.resp.Extra, r.ans.Additional...)
	if r.ans.Extended.Present {
		r.ede = wire.EDNS0_EDE{InfoCode: r.ans.Extended.Code, ExtraText: r.ans.Extended.Text}
		r.hasEDE = true
	}

	r.echoCase(q.Name)
}

// echoCase rewrites the owner names in the answer section to the casing the
// client used, wherever they are the name it asked about (0x20 encoding).
func (r *Responder) echoCase(qname string) {
	if !hasUpper(qname) {
		return
	}
	for i, rr := range r.resp.Answer {
		if strings.EqualFold(rr.Header().Name, qname) {
			echoed := wire.Copy(rr)
			echoed.Header().Name = qname
			r.resp.Answer[i] = echoed
		}
	}
}

// hasUpper reports whether s holds an unescaped US-ASCII capital, which is the
// only way a query name can differ from the lowercase form everything is stored
// in (RFC 4343).
func hasUpper(s string) bool {
	for i := range len(s) {
		if c := s[i]; c >= 'A' && c <= 'Z' {
			return true
		}
	}
	return false
}

// finish attaches EDNS0 where the query asked for it, cuts the response down to
// what the transport carries, and packs it.
func (r *Responder) finish(reqOPT *wire.OPT, tr Transport, out []byte) ([]byte, error) {
	budget := wire.MaxMsgSize
	if tr == UDP {
		budget = wire.MinMsgSize
	}

	if reqOPT != nil {
		// RFC 6891 §6.1.1: a response to a query carrying an OPT carries one.
		// The size in it is what we are willing to receive, which is our own
		// ceiling and has nothing to do with what the requestor advertised.
		r.opt.Hdr = wire.RR_Header{Name: ".", Rrtype: wire.TypeOPT}
		r.opt.SetUDPSize(r.limits.MaxUDPResponse)
		r.opt.SetVersion(0)
		// DO stays clear. Claiming DNSSEC awareness while sending no signatures
		// is how a validating resolver is taught to distrust us.
		r.opt.Option = r.opt.Option[:0]
		if r.hasEDE {
			r.opt.Option = append(r.opt.Option, &r.ede)
		}
		r.resp.Extra = append(r.resp.Extra, &r.opt)

		if tr == UDP {
			advertised := int(reqOPT.UDPSize())
			if advertised < wire.MinMsgSize {
				// RFC 6891 §6.2.3: anything smaller is read as 512.
				advertised = wire.MinMsgSize
			}
			budget = min(advertised, int(r.limits.MaxUDPResponse))
		}
	}

	// Truncate drops the additional section first, then the authority section,
	// then answers, and sets TC only if it had to drop something, which is the
	// order RFC 1035 §4.1.1 and architecture §2.7 ask for. It also
	// decides whether to compress: off for a message that already fits, so the
	// common answer neither compresses nor allocates the map that would take,
	// and on only where compression is what makes the response fit. Setting
	// Compress here would be overwritten either way.
	//
	// On TCP the budget is the largest message a two-octet length prefix can
	// frame (RFC 1035 §4.2.2). It is not a truncation policy: nothing this
	// server builds approaches 64 KiB, and the call is there so that an RRset
	// that somehow did could still be sent as something rather than as garbage.
	r.resp.Truncate(budget)

	packed, err := r.resp.PackBuffer(out)
	if err != nil {
		// Everything in the response was either built here or came out of the
		// wire library, so a failure is a bug on this side rather than
		// something a client did.
		return nil, fmt.Errorf("pack the response: %w", err)
	}

	r.ev.Rcode = r.resp.Rcode
	r.ev.Truncated = r.resp.Truncated
	r.ev.Dropped = false

	return packed, nil
}
