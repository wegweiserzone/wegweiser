package dns

import (
	"crypto/subtle"
	"encoding/binary"
	"net/netip"
	"time"

	wire "github.com/miekg/dns"
)

// A DNS Cookie is eight octets the client chose, optionally followed by eight
// to thirty-two octets the server chose (RFC 7873 §5.2). Anything else is
// malformed, and the lengths are the first thing checked about one.
const (
	clientCookieLen = 8
	minServerCookie = 8
	maxServerCookie = 32

	// serverCookieLen is what this server produces: the version, reserved and
	// timestamp sub-fields and the hash, laid out by RFC 9018 §4.4.
	serverCookieLen = 16

	// cookieLen is therefore every cookie this server sends: the client half
	// as it arrived and our own behind it, 24 octets in all.
	cookieLen = clientCookieLen + serverCookieLen

	// cookieVersion is the only version RFC 9018 defines. A cookie arriving
	// with another one was not written by us, whatever else is true of it.
	cookieVersion = 1
)

// How long a Server Cookie is worth (RFC 9018 §4.3), in seconds.
const (
	// cookieMaxAge is how far into the past a cookie is still taken. An hour,
	// so that a client asking one question an hour is not made to start over
	// every time.
	cookieMaxAge = 3600

	// cookieMaxSkew is how far into the future a cookie is still taken, for
	// the clocks of an anycast set that do not quite agree.
	cookieMaxSkew = 300

	// cookieRenewAge is when a cookie that is still valid is replaced with a
	// fresh one anyway, so that a client in a long conversation never arrives
	// holding an expired one.
	cookieRenewAge = 1800
)

// CookieSecret is the key Server Cookies are computed under.
//
// It is 128 bits because SipHash-2-4 takes a 128-bit key, and RFC 9018 §6
// leaves no second choice of hash. The secret never leaves this server: a
// cookie is meaningless anywhere else, which is what makes rotating it a
// decision this server can take on its own.
type CookieSecret [16]byte

// CookieSecrets are the secrets this server recognises its own cookies by.
//
// Two, because a rotation cannot be instantaneous: a client holds a Server
// Cookie for up to an hour (RFC 9018 §4.3), and the secret those were built
// under is still worth checking against. Where they come from and when they
// change is the control plane's business, not this package's.
type CookieSecrets struct {
	// Current is what a cookie handed out now is computed under.
	Current CookieSecret
	// Previous is the secret before it, or zero on a server that has never
	// rotated.
	Previous CookieSecret
}

// usable reports whether cookies can be issued at all, which they cannot
// before the control plane has published a secret.
func (s CookieSecrets) usable() bool { return s.Current != CookieSecret{} }

// knows reports whether server is a Server Cookie this server produced for
// this client and address, under either secret.
//
// The predecessor is checked second because the overwhelming majority of
// cookies are current, and checking it at all is what makes a rotation
// invisible to a client holding one from before it.
func (s CookieSecrets) knows(client, server []byte, ip netip.Addr) bool {
	if s.Current.matches(client, server, ip) {
		return true
	}
	return s.Previous != CookieSecret{} && s.Previous.matches(client, server, ip)
}

// cookieHolder wraps the secrets so they can be swapped atomically, the way
// the keyring is. A struct cannot be replaced field by field while a query is
// being answered with it.
type cookieHolder struct{ secrets CookieSecrets }

// serverCookie returns the Server Cookie for a client cookie, an address and a
// timestamp.
//
// The timestamp is seconds since the UNIX epoch modulo 2**32, which is what
// goes on the wire, and what makes a cookie expire without this server keeping
// a note about anyone.
func (s *CookieSecret) serverCookie(client []byte, ip netip.Addr, ts uint32) [serverCookieLen]byte {
	var out [serverCookieLen]byte
	// Octets 1 to 3 are reserved and are sent as zero (RFC 9018 §4.4).
	out[0] = cookieVersion
	binary.BigEndian.PutUint32(out[4:], ts)
	binary.LittleEndian.PutUint64(out[8:], s.hash(client, [4]byte(out[:4]), ts, ip))
	return out
}

// matches reports whether server is a Server Cookie this secret produced for
// this client cookie and this address.
//
// It says nothing about age: a cookie can be ours and hours old, and the two
// questions have different answers (RFC 9018 §4.3 renews one it still accepts).
func (s *CookieSecret) matches(client, server []byte, ip netip.Addr) bool {
	if len(server) != serverCookieLen || server[0] != cookieVersion {
		return false
	}
	ts := binary.BigEndian.Uint32(server[4:])

	var want [8]byte
	binary.LittleEndian.PutUint64(want[:], s.hash(client, [4]byte(server[:4]), ts, ip))

	// Constant time, because a client that can learn how much of its guess was
	// right learns the hash one octet at a time.
	return subtle.ConstantTimeCompare(want[:], server[8:]) == 1
}

// hash computes the hash sub-field over the input RFC 9018 §4.4 prescribes:
// the client cookie, the version and reserved octets, the timestamp and the
// client address, in that order and in wire form.
//
// meta is the version octet and the three reserved ones as they stand, which
// for a cookie arriving means as the client returned them. We send them zero,
// but a server in an anycast set may not, and the hash covers them either way.
func (s *CookieSecret) hash(client []byte, meta [4]byte, ts uint32, ip netip.Addr) uint64 {
	// Twenty octets for an IPv4 client and thirty-two for an IPv6 one, so the
	// buffer is the larger of the two and never escapes.
	var buf [32]byte

	n := copy(buf[:], client)
	n += copy(buf[n:], meta[:])
	binary.BigEndian.PutUint32(buf[n:], ts)
	n += 4

	// An address that reached a dual-stack socket as ::ffff:198.51.100.100 is
	// the same client as one that reached an IPv4 socket, and hashing the two
	// differently would hand it a cookie it cannot use.
	if ip.Is4() || ip.Is4In6() {
		a := ip.As4()
		n += copy(buf[n:], a[:])
	} else {
		a := ip.As16()
		n += copy(buf[n:], a[:])
	}

	k0 := binary.LittleEndian.Uint64(s[0:8])
	k1 := binary.LittleEndian.Uint64(s[8:16])
	return sipHash24(k0, k1, buf[:n])
}

// cookieAge returns how many seconds old ts is at now, in the modulo-2**32
// arithmetic RFC 9018 §4.3 asks for. A timestamp from the future gives a
// negative age, which is the case the clock skew allowance is about.
func cookieAge(ts, now uint32) int32 { return int32(now - ts) }

// cookieUsable reports whether a Server Cookie stamped at ts is still one this
// server honours at now.
func cookieUsable(ts, now uint32) bool {
	age := cookieAge(ts, now)
	return age <= cookieMaxAge && age >= -cookieMaxSkew
}

// cookieWorn reports whether a Server Cookie stamped at ts is old enough that
// the client should be handed a fresh one, though the one it sent still holds.
func cookieWorn(ts, now uint32) bool { return cookieAge(ts, now) >= cookieRenewAge }

// splitCookie divides a cookie option into the client and server halves.
//
// A cookie is either eight octets, meaning the client has not been given a
// Server Cookie yet, or between sixteen and forty, meaning it is returning one.
// Any other length is malformed and RFC 7873 §5.2.2 answers it with FORMERR,
// which is why this reports the difference rather than shrugging at it.
func splitCookie(opt []byte) (client, server []byte, ok bool) {
	switch {
	case len(opt) == clientCookieLen:
		return opt, nil, true
	case len(opt) >= clientCookieLen+minServerCookie &&
		len(opt) <= clientCookieLen+maxServerCookie:
		return opt[:clientCookieLen], opt[clientCookieLen:], true
	default:
		return nil, nil, false
	}
}

// cookieNow is the timestamp a cookie minted at t carries: seconds since the
// UNIX epoch modulo 2**32 (RFC 9018 §4.4). The truncation is the format, not a
// loss of precision to be sorry about.
func cookieNow(t time.Time) uint32 { return uint32(t.Unix()) }

// SetCookieSecrets publishes the secrets Server Cookies are computed and
// checked under, for every query answered from now on.
//
// The control plane mints and rotates them; this is where the query path finds
// out. A rotation reaches the next query rather than the next restart, which
// is what lets the previous secret be forgotten on a schedule instead of when
// somebody remembers to restart the server.
func (s *Server) SetCookieSecrets(secrets CookieSecrets) {
	s.cookies.Store(&cookieHolder{secrets: secrets})
}

// readCookie reads the cookie option the query carries, works out what goes
// back in the response, and reports whether what arrived was well formed.
//
// A query carrying no cookie is answered without one. RFC 7873 §5.2 leaves the
// exchange to the client to open, and a server volunteering 28 octets of
// option to everybody would be paying the cost of a defence nobody had asked
// to take part in.
func (r *Responder) readCookie(reqOPT *wire.OPT, from netip.Addr) bool {
	if reqOPT == nil {
		return true
	}
	opt := requestCookie(reqOPT)
	if opt == nil {
		return true
	}

	raw, ok := decodeCookieHex(opt.Cookie, r.cookieIn[:])
	if !ok {
		return false
	}
	client, server, ok := splitCookie(raw)
	if !ok {
		return false
	}

	// Before the control plane has published a secret there is nothing to
	// compute a cookie with, so the query is answered without one. That is the
	// same thing a client sees from a server implementing no cookies at all,
	// which is a case every client already handles.
	secrets := r.secrets()
	if !secrets.usable() {
		return true
	}

	now := cookieNow(time.Now())
	var ours, worn bool
	if secrets.knows(client, server, from) {
		ts := binary.BigEndian.Uint32(server[4:])
		ours, worn = cookieUsable(ts, now), cookieWorn(ts, now)
	}

	if ours && !worn {
		// Ours, and not old enough to be worth replacing: the client keeps
		// what it has. RFC 9018 §4.3 asks for a fresh one past half an hour,
		// which is early enough that a cookie never expires mid-conversation.
		copy(r.cookieOut[:], raw)
	} else {
		copy(r.cookieOut[:], client)
		issued := secrets.Current.serverCookie(client, from, now)
		copy(r.cookieOut[clientCookieLen:], issued[:])
	}
	r.hasCookie = true
	return true
}

// secrets returns what the server has published, or none where nothing has
// been published yet.
func (r *Responder) secrets() CookieSecrets {
	if r.cookies == nil {
		return CookieSecrets{}
	}
	if holder := r.cookies.Load(); holder != nil {
		return holder.secrets
	}
	return CookieSecrets{}
}

// requestCookie returns the cookie option of a query, or nil where it carries
// none.
func requestCookie(opt *wire.OPT) *wire.EDNS0_COOKIE {
	for _, o := range opt.Option {
		if cookie, ok := o.(*wire.EDNS0_COOKIE); ok {
			return cookie
		}
	}
	return nil
}

// decodeCookieHex decodes the hex the wire library hands a cookie over as into
// dst, and reports whether it was hex of a length dst holds.
//
// The library encodes what it read off the wire and decodes it again to pack a
// response, which is its interface rather than a choice of ours. Decoding into
// a buffer the responder already owns is what keeps reading one out of an
// allocation (D12).
func decodeCookieHex(s string, dst []byte) ([]byte, bool) {
	if len(s)%2 != 0 || len(s)/2 > len(dst) {
		return nil, false
	}
	for i := 0; i < len(s); i += 2 {
		hi, hiOK := unhex(s[i])
		lo, loOK := unhex(s[i+1])
		if !hiOK || !loOK {
			return nil, false
		}
		dst[i/2] = hi<<4 | lo
	}
	return dst[:len(s)/2], true
}

// unhex reads one hex digit.
func unhex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	default:
		return 0, false
	}
}
