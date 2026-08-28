package dns

import (
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	wire "github.com/miekg/dns"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// tsigFudge is how far apart the two clocks may be, in seconds. RFC 8945 §4.2
// leaves it to the signer and §5.2.3 works from 300, which every other
// implementation uses.
const tsigFudge = 300

// tsigReserve is the room a signature takes in a response, in octets: a key
// name, an algorithm name, the timers and a 32 octet MAC. Rounded up, and
// taken off the budget before truncation because a TSIG is not itself
// something to truncate.
const tsigReserve = 160

// TSIGKey is what signing and verifying need, and nothing else.
//
// The query path may not reach the database (invariant 2), so the wiring
// resolves a key and hands over this much of it.
type TSIGKey struct {
	Name      zone.Name
	Algorithm zone.TSIGAlgorithm
	Secret    []byte
}

// base64Secret is the form the wire library takes a secret in.
func (k TSIGKey) base64Secret() string { return base64.StdEncoding.EncodeToString(k.Secret) }

// Keyring is the TSIG keys this server holds, by name.
//
// Published rather than looked up. A signed query has to be verified before it
// is answered, and reading the database there would put a disk on the path of
// every query (invariant 2). The wiring reads the keys once and republishes
// them whenever one is created or withdrawn, the way the transfer list works.
//
// A key that has been withdrawn is not in it: docs/decisions/d28-tsig.md clears its
// secret, so there would be nothing to verify against.
type Keyring map[zone.Name]TSIGKey

// Key returns the key of that name, or false.
func (k Keyring) Key(name zone.Name) (TSIGKey, bool) {
	key, held := k[name]
	return key, held
}

// tsigResult is what verifying a request came to.
type tsigResult struct {
	// key is the name the request signed with, zero when it carried no TSIG.
	key zone.Name
	// mac is the request's MAC, which the first response signs over
	// (RFC 8945 §5.3).
	mac string
	// signer signs each message of the answer, and is nil for an unsigned
	// request.
	signer *tsigSigner
	// rr is the TSIG the request carried, kept for the refusals that echo it.
	rr *wire.TSIG
}

// tsigError is a refusal RFC 8945 §5.2 spells out, carried back to the client
// in a TSIG record of its own.
type tsigError struct {
	code uint16
	text string
	// rr is the TSIG the request carried, which the refusal echoes back.
	rr *wire.TSIG
	// key is set only where the refusal is signed, which is BADTIME alone.
	key *TSIGKey
}

func (e *tsigError) Error() string { return e.text }

// verifyTSIG checks the signature on a request, if it carries one.
//
// An unsigned request is not a failure here: it is a request that has named no
// key, and whether that is enough is the transfer list's question, not this
// one's.
func verifyTSIG(ring Keyring, query []byte, req *wire.Msg) (tsigResult, error) {
	rr := req.IsTsig()
	if rr == nil {
		return tsigResult{}, nil
	}

	name, err := zone.ParseName(rr.Hdr.Name)
	if err != nil {
		return tsigResult{}, &tsigError{code: wire.RcodeBadKey, text: "that is not a key name", rr: rr}
	}

	key, held := ring.Key(name)
	// An unknown name and a withdrawn key leave by the same door. Which of the
	// two it was is not a client's business, and RFC 8945 §5.2.1 has one code
	// for both.
	if !held {
		return tsigResult{}, &tsigError{code: wire.RcodeBadKey, text: "no key of that name signs here", rr: rr}
	}
	if key.Algorithm != zone.TSIGAlgorithm(wire.CanonicalName(rr.Algorithm)) {
		return tsigResult{}, &tsigError{
			code: wire.RcodeBadKey,
			text: "that key does not sign with " + rr.Algorithm,
			rr:   rr,
		}
	}

	switch verr := wire.TsigVerify(query, key.base64Secret(), "", false); {
	case verr == nil:
	case errors.Is(verr, wire.ErrTime):
		// The one of the three a client can act on, so it is told what this
		// server's clock says (RFC 8945 §5.2.3).
		return tsigResult{}, &tsigError{
			code: wire.RcodeBadTime,
			text: "the time you signed at is outside this server's window",
			rr:   rr,
			key:  &key,
		}
	default:
		return tsigResult{}, &tsigError{code: wire.RcodeBadSig, text: "that signature does not verify", rr: rr}
	}

	return tsigResult{
		key:    name,
		mac:    rr.MAC,
		rr:     rr,
		signer: &tsigSigner{key: key, prev: rr.MAC},
	}, nil
}

// tsigSigner signs the messages of one answer, in order.
//
// It is stateful because RFC 8945 §5.3 makes each digest after the first depend
// on the MAC of the message before it, and it belongs to one exchange rather
// than to the server.
type tsigSigner struct {
	key TSIGKey
	// prev is the MAC the next message signs over: the request's to begin
	// with, then each response's in turn.
	prev string
	// sent is whether the first message has gone. From the second on only the
	// timers are covered rather than every TSIG variable (RFC 8945 §5.3.1).
	sent bool
}

// sign packs a message with a TSIG record appended.
func (s *tsigSigner) sign(m *wire.Msg) ([]byte, error) {
	m.SetTsig(s.key.Name.String(), s.key.Algorithm.String(), tsigFudge, time.Now().Unix())

	packed, mac, err := wire.TsigGenerate(m, s.key.base64Secret(), s.prev, s.sent)
	if err != nil {
		return nil, fmt.Errorf("sign a message for %s: %w", s.key.Name, err)
	}
	s.prev, s.sent = mac, true
	return packed, nil
}

// signRefusal packs a refusal with the TSIG record RFC 8945 §5.2 asks for.
//
// BADKEY and BADSIG go back unsigned, because this server either has no key to
// sign with or has just decided the client does not hold the one it named
// (§5.2.1, §5.2.2). BADTIME is signed and carries this server's clock in the
// other data, which is what lets the client see how far apart the two are
// (§5.2.3).
func signRefusal(m *wire.Msg, req *wire.TSIG, key *TSIGKey, code uint16, now time.Time) ([]byte, error) {
	rr := &wire.TSIG{
		Hdr: wire.RR_Header{
			Name: req.Hdr.Name, Rrtype: wire.TypeTSIG, Class: wire.ClassANY,
		},
		Algorithm:  req.Algorithm,
		TimeSigned: req.TimeSigned,
		Fudge:      tsigFudge,
		OrigId:     m.Id,
		Error:      code,
	}
	if code == wire.RcodeBadTime {
		// Six octets of this server's clock, hex-encoded the way the wire
		// library takes other data (RFC 8945 §5.2.3).
		rr.OtherLen = 6
		rr.OtherData = fmt.Sprintf("%012x", now.Unix())
	}
	m.Extra = append(m.Extra, rr)

	if key == nil {
		// Nothing to sign with, so the record goes out as it stands. Packed
		// here rather than through the library, which zeroes the time signed
		// on an unsigned refusal: a client then reads back nothing where it
		// sent a time, and says the clocks disagree when they do not.
		packed, err := m.Pack()
		if err != nil {
			return nil, fmt.Errorf("pack a refusal for %s: %w", req.Hdr.Name, err)
		}
		return packed, nil
	}

	packed, _, err := wire.TsigGenerate(m, key.base64Secret(), "", false)
	if err != nil {
		return nil, fmt.Errorf("pack a refusal for %s: %w", req.Hdr.Name, err)
	}
	return packed, nil
}

// keyHolder wraps the keyring so it can be swapped atomically, the way the
// transfer policy is. A map cannot be the element of an [atomic.Pointer].
type keyHolder struct{ ring Keyring }

// SetKeys publishes the keys this server verifies and signs with, for every
// request from here on.
func (s *Server) SetKeys(ring Keyring) { s.keys.Store(&keyHolder{ring: ring}) }

// keyring is what the current request is checked against, and is empty rather
// than nil for a server nobody has given keys to.
func (s *Server) keyring() Keyring {
	if h := s.keys.Load(); h != nil {
		return h.ring
	}
	return nil
}
