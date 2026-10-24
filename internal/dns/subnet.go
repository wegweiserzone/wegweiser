package dns

import (
	"encoding/binary"
	"net/netip"
	"slices"

	wire "github.com/miekg/dns"
)

// The client subnet option (RFC 7871), which a resolver in front of this
// server can use to say whom it is asking for (docs/decisions/ D50). It is
// supported as RFC 7871 asks of an authority, on every query that carries it,
// and believed only from a resolver on the list the server is handed. Nothing
// about an answer depends on it.

// subnetFamilyIPv4 and subnetFamilyIPv6 are the two families RFC 7871 §6
// defines, by their IANA Address Family Numbers.
const (
	subnetFamilyIPv4 = 1
	subnetFamilyIPv6 = 2
)

// subnetHeaderLen is the family, the source prefix length and the scope prefix
// length that come before the address (RFC 7871 §6).
const subnetHeaderLen = 4

// resolverHolder wraps the list so it can be swapped atomically.
type resolverHolder struct{ prefixes []netip.Prefix }

// SetResolvers publishes the resolvers whose word on a client is believed, for
// every query from here on. An empty list believes nobody, which is where a
// server starts.
func (s *Server) SetResolvers(prefixes []netip.Prefix) {
	s.resolvers.Store(&resolverHolder{prefixes: slices.Clone(prefixes)})
}

// believes reports whether a query from this address may name its client.
func (r *Responder) believes(from netip.Addr) bool {
	if r.resolvers == nil {
		return false
	}
	h := r.resolvers.Load()
	if h == nil {
		return false
	}
	from = from.Unmap()
	for _, p := range h.prefixes {
		if p.Contains(from) {
			return true
		}
	}
	return false
}

// requestSubnet returns the client subnet option a query carries, or nil.
func requestSubnet(opt *wire.OPT) *wire.EDNS0_SUBNET {
	for _, o := range opt.Option {
		if s, ok := o.(*wire.EDNS0_SUBNET); ok {
			return s
		}
	}
	return nil
}

// readSubnet reads the client subnet option of a query, prepares the one the
// response carries back, and notes the client for whoever is watching when the
// query comes from a resolver this server believes.
//
// It reports false for an option whose address is longer or shorter than its
// source prefix calls for, which RFC 7871 §6 asks to be answered FORMERR. An
// unknown family or a prefix longer than the family allows never gets here:
// the wire library refuses the whole message for those, and that is answered
// FORMERR too (§7.2.1).
func (r *Responder) readSubnet(reqOPT *wire.OPT, query []byte, from netip.Addr) bool {
	if reqOPT == nil {
		return true
	}
	opt := requestSubnet(reqOPT)
	if opt == nil {
		return true
	}

	// The library copies whatever address octets there are and keeps no
	// length, so the option is measured where it sits in the message.
	length, found := subnetOptionLength(query)
	if !found || length != subnetHeaderLen+(int(opt.SourceNetmask)+7)/8 {
		return false
	}

	// Sent back as it came, with a scope of 0: the answer is the same for
	// every client, and saying so is what keeps a resolver's cache shared
	// (RFC 7871 §7.2.1, §12.1, and §7.4 for a delegation).
	r.subnet = *opt
	r.subnet.SourceScope = 0
	r.hasSubnet = true

	if r.believes(from) {
		r.ev.For = subnetPrefix(opt)
	}
	return true
}

// subnetPrefix is the client an option names, or the zero prefix when it names
// none: a source prefix of 0 is a client that asked not to be named
// (RFC 7871 §7.1.2).
func subnetPrefix(opt *wire.EDNS0_SUBNET) netip.Prefix {
	if opt.SourceNetmask == 0 {
		return netip.Prefix{}
	}
	var addr netip.Addr
	switch opt.Family {
	case subnetFamilyIPv4:
		v4 := opt.Address.To4()
		if v4 == nil {
			return netip.Prefix{}
		}
		addr = netip.AddrFrom4([4]byte(v4))
	case subnetFamilyIPv6:
		v6 := opt.Address.To16()
		if v6 == nil {
			return netip.Prefix{}
		}
		addr = netip.AddrFrom16([16]byte(v6))
	default:
		return netip.Prefix{}
	}
	// The address is meant to be cut to its prefix already (RFC 7871 §6);
	// masking keeps a resolver that left bits in from naming more than it said.
	return netip.PrefixFrom(addr, int(opt.SourceNetmask)).Masked()
}

// subnetOptionLength finds the client subnet option in a query and returns the
// length its option header gives it.
//
// It walks a message the wire library has already unpacked whole, so the
// structure holds, and every step is bounded anyway: this is the path a
// malformed packet could reach. It allocates nothing.
func subnetOptionLength(msg []byte) (int, bool) {
	if len(msg) < headerLen {
		return 0, false
	}
	questions := int(binary.BigEndian.Uint16(msg[4:]))
	records := int(binary.BigEndian.Uint16(msg[6:])) +
		int(binary.BigEndian.Uint16(msg[8:])) +
		int(binary.BigEndian.Uint16(msg[10:]))

	off := headerLen
	for range questions {
		next, ok := skipName(msg, off)
		if !ok || next+4 > len(msg) {
			return 0, false
		}
		off = next + 4
	}
	for range records {
		next, ok := skipName(msg, off)
		// Type, class, TTL and the data length: ten octets.
		if !ok || next+10 > len(msg) {
			return 0, false
		}
		rrtype := binary.BigEndian.Uint16(msg[next:])
		dataLen := int(binary.BigEndian.Uint16(msg[next+8:]))
		data := next + 10
		if data+dataLen > len(msg) {
			return 0, false
		}
		if rrtype == wire.TypeOPT {
			return findOption(msg[data:data+dataLen], wire.EDNS0SUBNET)
		}
		off = data + dataLen
	}
	return 0, false
}

// findOption returns the length of the first option with this code in the
// data of an OPT record (RFC 6891 §6.1.2).
func findOption(data []byte, code uint16) (int, bool) {
	for len(data) >= 4 {
		c := binary.BigEndian.Uint16(data)
		n := int(binary.BigEndian.Uint16(data[2:]))
		if 4+n > len(data) {
			return 0, false
		}
		if c == code {
			return n, true
		}
		data = data[4+n:]
	}
	return 0, false
}

// skipName returns the offset just past the name starting at off: past its
// root label, or past the pointer that ends it (RFC 1035 §4.1.4).
func skipName(msg []byte, off int) (int, bool) {
	for off < len(msg) {
		label := msg[off]
		switch {
		case label == 0:
			return off + 1, true
		case label&0xC0 == 0xC0:
			return off + 2, off+2 <= len(msg)
		case label&0xC0 != 0:
			return 0, false
		default:
			off += 1 + int(label)
		}
	}
	return 0, false
}
