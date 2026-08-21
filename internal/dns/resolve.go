package dns

import (
	wire "github.com/miekg/dns"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// maxCNAMEChain is the number of CNAME records one answer may carry.
//
// RFC 1034 §4.3.2 warns that a chain can loop but names no limit, so this is a
// choice. Eight is past any chain built on purpose, and stopping there turns a
// loop in our own data into an error rather than an unbounded walk.
const maxCNAMEChain = 8

// maxAdditional is how many records the additional section may be filled with.
// They are a convenience, so the bound is on how much a response may grow for
// them rather than on how useful they could be.
const maxAdditional = 16

// Question is one question out of a query.
//
// The name is already lowercased, since [zone.Name] holds every name that way
// (RFC 4343). Echoing the client's casing back is the message layer's job, so
// it never reaches the resolver.
type Question struct {
	Name  zone.Name
	Class zone.Class
	Type  zone.RRType
}

// ExtendedError is an extended DNS error (RFC 8914): a code and a short text
// saying why a response looks the way it does.
type ExtendedError struct {
	// Present tells an unset error apart from one carrying code 0, which
	// RFC 8914 §4.1 gives the meaning "Other" rather than "none".
	Present bool
	// Code is the INFO-CODE of RFC 8914 §2.
	Code uint16
	// Text is the EXTRA-TEXT: one operator-facing sentence, never a
	// machine-readable payload.
	Text string
}

// Answer is a resolved question in the form of records: decided, not encoded.
// Keeping the two apart makes the search of RFC 1034 §4.3.2 a pure function of
// a snapshot and a question, testable without a socket.
//
// The caller owns the value and is meant to reuse it. [Answer.Reset] empties
// the sections without releasing their memory, so a server holding one per
// worker allocates nothing per query (D12).
type Answer struct {
	// Rcode is the response code, numbered as the wire library numbers it.
	Rcode int

	// Authoritative is the AA bit. It says this server is an authority for the
	// name in the question (RFC 1035 §4.1.1): for that name, not for wherever
	// a CNAME chain happened to end.
	Authoritative bool

	// The three sections of a response. Their records belong to the snapshot
	// and are shared with every other query reading it: a caller may reorder or
	// drop them, and may never write to one.
	Answer     []wire.RR
	Authority  []wire.RR
	Additional []wire.RR

	// Extended explains a response that an RCODE alone would not (RFC 8914).
	Extended ExtendedError
}

// Reset empties the answer for reuse, keeping the memory its sections have
// already claimed.
func (a *Answer) Reset() {
	*a = Answer{
		Answer:     a.Answer[:0],
		Authority:  a.Authority[:0],
		Additional: a.Additional[:0],
	}
}

// explain attaches an extended error to the answer (RFC 8914).
func (a *Answer) explain(code uint16, text string) {
	a.Extended = ExtendedError{Present: true, Code: code, Text: text}
}

// Resolve answers q from the snapshot, writing the result into a.
//
// This is the canonical name search of RFC 1034 §4.3.2: delegation before data,
// wildcards only where no closer name exists, NODATA where a name exists
// without the type asked for, NXDOMAIN only where the name exists nowhere.
// Nothing here reads the clock, the network or the database.
//
// a is reset first, so the same one can be passed on every query. The records
// it points at belong to the snapshot and must not be modified.
func (s *Snapshot) Resolve(q Question, a *Answer) {
	a.Reset()

	// AXFR and IXFR ask for a transfer, MAILA and MAILB for records that were
	// obsolete before this server existed, and OPT, TSIG and TKEY belong to a
	// message rather than to a zone (RFC 6895 §3.1). ANY is the one QTYPE that
	// asks about zone data, and it is answered below.
	if q.Type.IsMeta() || (q.Type.IsQueryOnly() && q.Type != zone.TypeANY) {
		a.Rcode = wire.RcodeNotImplemented
		a.explain(wire.ExtendedErrorCodeNotSupported,
			"this server answers questions about zone data only")
		return
	}

	t := s.zoneFor(q.Name)
	if t == nil {
		// Refused, not NXDOMAIN: a server cannot assert that a name does not
		// exist in a namespace it does not serve. Asserting it anyway is the
		// classic lame-server bug (architecture §2.4).
		a.Rcode = wire.RcodeRefused
		a.explain(wire.ExtendedErrorCodeNotAuthoritative,
			"no zone on this server covers that name")
		return
	}
	a.Authoritative = true

	name := q.Name
	chased := 0
	for {
		l := t.lookup(name)

		// Before anything else: at or below a delegation this zone's records
		// are not ours to answer with, whatever they say. The node at the
		// delegation point exists and carries data, and that is exactly the
		// trap: the answer is still a referral (RFC 1034 §4.3.2 step 3b).
		if l.delegation != nil {
			// The AA bit speaks about the name in the question. It is off when
			// that name is the one sitting below the delegation, and stays on
			// when a CNAME we were authoritative for led here.
			a.Authoritative = chased > 0
			t.referral(l.delegation, q, a)
			return
		}

		n := l.node
		if n == nil {
			// The name is not there. A "*" one label below the closest
			// encloser is the last thing that can answer it (RFC 4592 §3.3.1).
			// Both are worked out at build time, so ruling a wildcard out
			// costs a nil check.
			if l.closest == nil || l.closest.wild == nil {
				t.nxdomain(a)
				return
			}
			n = l.closest.wild
		}

		target, chase := t.answerAt(n, name, q, a)
		if !chase {
			return
		}

		chased++
		if chased > maxCNAMEChain {
			// A chain this long is a loop or a mistake in data we serve. Handing
			// back what we have with NOERROR would only push the loop onto the
			// resolver, which would ask us again and get the same thing.
			a.Reset()
			a.Rcode = wire.RcodeServerFailure
			a.explain(wire.ExtendedErrorCodeOther,
				"the CNAME chain at this name is longer than this server follows")
			return
		}

		name = target
		if t = s.zoneFor(name); t == nil {
			// The chain left every zone we serve. What we have is authoritative
			// as far as it goes, and following it further is a resolver's job
			// (RFC 1034 §4.3.2 step 3a).
			return
		}
	}
}

// answerAt fills the answer from the records at n (either the name itself or
// the wildcard being synthesised for it) and reports the CNAME target when
// resolution has to continue somewhere else.
func (t *zoneTree) answerAt(n *node, owner zone.Name, q Question, a *Answer) (zone.Name, bool) {
	if q.Type == zone.TypeANY {
		set := anyRRset(n, q.Class)
		if set == nil {
			t.nodata(a)
			return zone.Name{}, false
		}
		a.appendAnswer(set, n.name, owner)
		t.addAddresses(set, nil, a)
		return zone.Name{}, false
	}

	if set := n.find(q.Class, q.Type); set != nil {
		a.appendAnswer(set, n.name, owner)
		t.addAddresses(set, nil, a)
		return zone.Name{}, false
	}

	// RFC 1034 §4.3.2 step 3a. A CNAME shares its name with nothing else
	// (RFC 2181 §10.1), so reaching here means the question asked for another
	// type; one for the CNAME itself was answered above.
	//
	// TODO: DNAME (RFC 6672) belongs here too. A DNAME above the name
	// synthesises a CNAME that then chases like any other.
	if set := n.find(q.Class, zone.TypeCNAME); set != nil {
		a.appendAnswer(set, n.name, owner)
		if len(set.targets) == 0 {
			// The builder parses a target for every CNAME it stores, so this is
			// a zone assembled by hand. Answering the CNAME and stopping is
			// what we do for a target outside our zones anyway.
			return zone.Name{}, false
		}
		return set.targets[0], true
	}

	t.nodata(a)
	return zone.Name{}, false
}

// anyRRset picks the one RRset a QTYPE=ANY question is answered with.
//
// RFC 8482 §4.1 allows answering ANY with a subset, and one RRset is the
// smallest subset that is still true. Returning everything at a name is an
// amplification lever: one small question, one large answer. Inspecting a name
// is what the record editor, the CLI and the query stream are for.
//
// The choice is deterministic, so an answer never depends on the order records
// arrived in: a CNAME if there is one, since RFC 2181 §10.1 leaves nothing else
// at that name, and otherwise the lowest type number present.
func anyRRset(n *node, c zone.Class) *rrset {
	var best *rrset
	for i := range n.sets {
		set := &n.sets[i]
		if set.class != c {
			continue
		}
		if set.typ == zone.TypeCNAME {
			return set
		}
		if best == nil || set.typ < best.typ {
			best = set
		}
	}
	return best
}

// referral hands the query on to the servers a delegation names (RFC 1034
// §4.3.2 step 3b): NOERROR, an empty answer section, the NS records in the
// authority section, and their addresses where this zone holds them. Without
// that glue, a referral to a name inside the delegated zone cannot be followed.
//
// TODO: a DS query at the delegation point is answered from the parent side
// rather than referred (RFC 4035 §3.1.4.1). DNSSEC is out of scope for v0.1.
func (t *zoneTree) referral(d *node, q Question, a *Answer) {
	a.Rcode = wire.RcodeSuccess
	// The class comes from the question rather than from the delegation,
	// because an answer never mixes classes. In v0.1 only IN reaches here at
	// all; the message layer refuses the rest (§2.2).
	if set := d.find(q.Class, zone.TypeNS); set != nil {
		a.Authority = append(a.Authority, set.rrs...)
		t.addAddresses(set, d, a)
	}
}

// addAddresses puts the addresses of the names in set into the additional
// section, so a resolver can use the answer without asking again (RFC 1034
// §4.3.2 step 6).
//
// glueFor is the delegation the records are being referred to, nil for ordinary
// data. An address we are authoritative for may always be sent; one below a
// delegation only as the glue of that same delegation. Without the distinction,
// a name below one delegation would leak out as the answer to an MX elsewhere
// in the zone, out of bailiwick.
func (t *zoneTree) addAddresses(set *rrset, glueFor *node, a *Answer) {
	switch set.typ {
	case zone.TypeNS, zone.TypeMX, zone.TypeSRV:
	default:
		return
	}

	for _, target := range set.targets {
		n := t.nodes[target]
		if n == nil || (n.delegation != nil && n.delegation != glueFor) {
			continue
		}
		for _, typ := range [...]zone.RRType{zone.TypeA, zone.TypeAAAA} {
			addrs := n.find(set.class, typ)
			// Stopping rather than skipping ahead keeps the section a prefix of
			// what was asked for, and keeps an RRset whole: half an address set
			// is a hint that reads like a complete one.
			if addrs == nil {
				continue
			}
			if len(a.Additional)+len(addrs.rrs) > maxAdditional {
				return
			}
			a.Additional = append(a.Additional, addrs.rrs...)
		}
	}
}

// nodata answers a name that exists without the type that was asked for, and
// also an empty non-terminal: a name that exists only because something below
// it does. Both are NOERROR with an empty answer section and never NXDOMAIN
// (RFC 4592 §2.2.2, RFC 8020).
func (t *zoneTree) nodata(a *Answer) {
	a.Rcode = wire.RcodeSuccess
	a.Authority = append(a.Authority, t.negSOA)
}

// nxdomain answers a name that does not exist and cannot be synthesised.
func (t *zoneTree) nxdomain(a *Answer) {
	a.Rcode = wire.RcodeNameError
	a.Authority = append(a.Authority, t.negSOA)
}

// appendAnswer puts an RRset in the answer section under owner rather than
// under the name it is stored at. The two differ only for a wildcard, where
// RFC 4592 §3.3.1 has the synthesised records carry the name that was asked
// for. Rewriting means copying, since the snapshot's records are shared with
// every other query. It is the resolver's only allocation, on that path alone.
func (a *Answer) appendAnswer(set *rrset, stored, owner zone.Name) {
	if stored.Equal(owner) {
		a.Answer = append(a.Answer, set.rrs...)
		return
	}

	name := owner.String()
	for _, rr := range set.rrs {
		synth := wire.Copy(rr)
		synth.Header().Name = name
		a.Answer = append(a.Answer, synth)
	}
}
