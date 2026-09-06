package dns

import (
	"crypto/subtle"
	"encoding/binary"
	"net/netip"
)

// A DNS Cookie is eight octets the client chose, optionally followed by eight
// to thirty-two octets the server chose (RFC 7873 §5.2). Anything else is
// malformed, and the lengths are the first thing checked about one.
const (
	clientCookieLen = 8
	minServerCookie = 8
	maxServerCookie = 32

	// serverCookieLen is what this server produces: the version, reserved and
	// timestamp sub-fields and the hash, laid out by RFC 9018 §4.4. With the
	// client half in front of it, every cookie this server sends is the 24
	// octets that section fixes.
	serverCookieLen = 16

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

// cookieSecret is the key Server Cookies are computed under.
//
// It is 128 bits because SipHash-2-4 takes a 128-bit key, and RFC 9018 §6
// leaves no second choice of hash. The secret never leaves this server: a
// cookie is meaningless anywhere else, which is what makes rotating it a local
// decision.
type cookieSecret [16]byte

// serverCookie returns the Server Cookie for a client cookie, an address and a
// timestamp.
//
// The timestamp is seconds since the UNIX epoch modulo 2**32, which is what
// goes on the wire, and what makes a cookie expire without this server keeping
// a note about anyone.
func (s *cookieSecret) serverCookie(client []byte, ip netip.Addr, ts uint32) [serverCookieLen]byte {
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
func (s *cookieSecret) matches(client, server []byte, ip netip.Addr) bool {
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
func (s *cookieSecret) hash(client []byte, meta [4]byte, ts uint32, ip netip.Addr) uint64 {
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
