package zone

import (
	"fmt"
	"math/bits"
	"net/netip"
	"strconv"
	"strings"
)

// The two namespaces that hold reverse mappings.
var (
	// ArpaV4 is in-addr.arpa., the IPv4 reverse namespace (RFC 1035 §3.5).
	ArpaV4 = MustParseName("in-addr.arpa.")
	// ArpaV6 is ip6.arpa., the IPv6 reverse namespace in nibble form
	// (RFC 3596 §2.5).
	ArpaV6 = MustParseName("ip6.arpa.")
)

// ErrNotReverse reports a name that lies outside both reverse namespaces.
var ErrNotReverse = fmt.Errorf("%w: not a reverse name", ErrInvalid)

// hexDigits is the alphabet of an ip6.arpa label, lower case because names are
// stored case-folded.
const hexDigits = "0123456789abcdef"

// IsReverseName reports whether n lies in one of the reverse namespaces.
func IsReverseName(n Name) bool {
	return n.IsSubDomainOf(ArpaV4) || n.IsSubDomainOf(ArpaV6)
}

// ReverseName returns the name that carries the PTR record for addr:
// "10.2.0.192.in-addr.arpa." for 192.0.2.10, and the nibble form of
// RFC 3596 §2.5 for an IPv6 address.
func ReverseName(addr netip.Addr) (Name, error) {
	if !addr.IsValid() {
		return Name{}, fmt.Errorf("%w address", ErrInvalid)
	}

	var b strings.Builder
	if addr.Is4() {
		octets := addr.As4()
		for i := len(octets) - 1; i >= 0; i-- {
			b.WriteString(strconv.Itoa(int(octets[i])))
			b.WriteByte('.')
		}
		b.WriteString(ArpaV4.String())
		return ParseName(b.String())
	}

	octets := addr.As16()
	for i := len(octets) - 1; i >= 0; i-- {
		b.WriteByte(hexDigits[octets[i]&0x0f])
		b.WriteByte('.')
		b.WriteByte(hexDigits[octets[i]>>4])
		b.WriteByte('.')
	}
	b.WriteString(ArpaV6.String())
	return ParseName(b.String())
}

// ReverseZoneName returns the zone name responsible for a network.
//
// A prefix on an octet boundary gets the ordinary form, "2.0.192.in-addr.arpa."
// for 192.0.2.0/24. A longer IPv4 prefix gets the classless form of RFC 2317 §4,
// "0/25.2.0.192.in-addr.arpa." for 192.0.2.0/25.
//
// An IPv6 prefix must fall on a nibble boundary, since ip6.arpa has no finer
// division to offer.
func ReverseZoneName(p netip.Prefix) (Name, error) {
	if !p.IsValid() {
		return Name{}, fmt.Errorf("%w prefix", ErrInvalid)
	}
	p = p.Masked()

	if p.Addr().Is4() {
		return reverseZoneNameV4(p)
	}

	if p.Bits()%4 != 0 {
		return Name{}, fmt.Errorf(
			"%w: an IPv6 reverse zone must end on a nibble boundary, so /%d is not "+
				"expressible; use /%d or /%d (RFC 3596 §2.5)",
			ErrInvalid, p.Bits(), p.Bits()/4*4, p.Bits()/4*4+4)
	}

	octets := p.Addr().As16()
	nibbles := p.Bits() / 4

	var b strings.Builder
	for i := nibbles - 1; i >= 0; i-- {
		n := octets[i/2]
		if i%2 == 0 {
			n >>= 4
		}
		b.WriteByte(hexDigits[n&0x0f])
		b.WriteByte('.')
	}
	b.WriteString(ArpaV6.String())
	return ParseName(b.String())
}

// reverseZoneNameV4 builds the in-addr.arpa zone name for an IPv4 network.
func reverseZoneNameV4(p netip.Prefix) (Name, error) {
	octets := p.Addr().As4()
	whole := p.Bits() / 8

	var b strings.Builder
	// A prefix that stops inside an octet needs the classless label of
	// RFC 2317, which names the octet's value and the full prefix length.
	if p.Bits()%8 != 0 {
		b.WriteString(strconv.Itoa(int(octets[whole])))
		b.WriteByte('/')
		b.WriteString(strconv.Itoa(p.Bits()))
		b.WriteByte('.')
	}
	for i := whole - 1; i >= 0; i-- {
		b.WriteString(strconv.Itoa(int(octets[i])))
		b.WriteByte('.')
	}
	b.WriteString(ArpaV4.String())
	return ParseName(b.String())
}

// ParseReversePrefix returns the network a reverse zone is responsible for.
//
// It understands the ordinary octet and nibble forms, and both spellings of the
// classless delegation of RFC 2317 §4: "0/25", which names the prefix length
// directly, and "0-127", the range form BIND setups commonly use.
func ParseReversePrefix(n Name) (netip.Prefix, error) {
	switch {
	case n.IsSubDomainOf(ArpaV4):
		return parseReversePrefixV4(n)
	case n.IsSubDomainOf(ArpaV6):
		return parseReversePrefixV6(n)
	default:
		return netip.Prefix{}, fmt.Errorf("%w: %q is under neither %s nor %s",
			ErrNotReverse, n, ArpaV4, ArpaV6)
	}
}

// parseReversePrefixV4 reads an in-addr.arpa zone name.
func parseReversePrefixV4(n Name) (netip.Prefix, error) {
	labels := labelsAbove(n, ArpaV4)
	if len(labels) > 4 {
		return netip.Prefix{}, fmt.Errorf(
			"%w: %q has %d labels under %s, and an IPv4 address has only four octets",
			ErrInvalid, n, len(labels), ArpaV4)
	}

	var (
		octets [4]byte
		prefix int
	)
	// Labels run from least to most significant, so read them backwards.
	for i := len(labels) - 1; i >= 0; i-- {
		label := labels[i]
		index := len(labels) - 1 - i

		if strings.ContainsAny(label, "/-") {
			// A classless label may only be the last octet named, since
			// nothing can be delegated below it.
			if i != 0 {
				return netip.Prefix{}, fmt.Errorf(
					"%w: %q puts the classless label %q above another octet (RFC 2317 §4)",
					ErrInvalid, n, label)
			}
			value, length, err := parseClasslessLabel(label, index)
			if err != nil {
				return netip.Prefix{}, fmt.Errorf("%w in %q: %w", ErrInvalid, n, err)
			}
			octets[index] = value
			prefix = length
			break
		}

		v, err := strconv.ParseUint(label, 10, 8)
		if err != nil || (len(label) > 1 && label[0] == '0') {
			return netip.Prefix{}, fmt.Errorf(
				"%w: %q in %q is not an octet between 0 and 255", ErrInvalid, label, n)
		}
		octets[index] = byte(v)
		prefix = (index + 1) * 8
	}

	return netip.PrefixFrom(netip.AddrFrom4(octets), prefix).Masked(), nil
}

// parseClasslessLabel reads the RFC 2317 label naming the last octet, in either
// the "0/25" or the "0-127" spelling, and returns the octet value and the full
// prefix length. index is the position of the octet the label stands for.
func parseClasslessLabel(label string, index int) (value byte, prefix int, err error) {
	base := index * 8

	if first, rest, ok := strings.Cut(label, "/"); ok {
		v, verr := strconv.ParseUint(first, 10, 8)
		length, lerr := strconv.Atoi(rest)
		if verr != nil || lerr != nil {
			return 0, 0, fmt.Errorf("classless label %q is not <octet>/<prefix length>", label)
		}
		if length <= base || length > base+8 {
			return 0, 0, fmt.Errorf(
				"classless label %q claims a /%d, which does not fall inside the octet it names",
				label, length)
		}
		return byte(v), length, nil
	}

	first, rest, ok := strings.Cut(label, "-")
	if !ok {
		return 0, 0, fmt.Errorf("classless label %q is neither <octet>/<prefix length> nor <first>-<last>", label)
	}
	lo, loErr := strconv.ParseUint(first, 10, 8)
	hi, hiErr := strconv.ParseUint(rest, 10, 8)
	if loErr != nil || hiErr != nil || hi < lo {
		return 0, 0, fmt.Errorf("classless label %q is not a range of octets", label)
	}

	// The range has to be one aligned block, or it names no prefix at all.
	size := hi - lo + 1
	if bits.OnesCount64(size) != 1 || lo%size != 0 {
		return 0, 0, fmt.Errorf(
			"classless label %q covers %d addresses starting at %d, which is not an aligned block",
			label, size, lo)
	}
	return byte(lo), base + 8 - bits.TrailingZeros64(size), nil
}

// parseReversePrefixV6 reads an ip6.arpa zone name in the nibble form of
// RFC 3596 §2.5.
func parseReversePrefixV6(n Name) (netip.Prefix, error) {
	labels := labelsAbove(n, ArpaV6)
	if len(labels) > 32 {
		return netip.Prefix{}, fmt.Errorf(
			"%w: %q has %d labels under %s, and an IPv6 address has only 32 nibbles",
			ErrInvalid, n, len(labels), ArpaV6)
	}

	var octets [16]byte
	for i := len(labels) - 1; i >= 0; i-- {
		label := labels[i]
		if len(label) != 1 {
			return netip.Prefix{}, fmt.Errorf(
				"%w: %q in %q is not a single hex digit; ip6.arpa names one nibble per label (RFC 3596 §2.5)",
				ErrInvalid, label, n)
		}
		v, err := strconv.ParseUint(label, 16, 8)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf(
				"%w: %q in %q is not a hex digit", ErrInvalid, label, n)
		}

		index := len(labels) - 1 - i
		if index%2 == 0 {
			octets[index/2] |= byte(v) << 4
		} else {
			octets[index/2] |= byte(v)
		}
	}

	return netip.PrefixFrom(netip.AddrFrom16(octets), len(labels)*4).Masked(), nil
}

// labelsAbove returns the labels of n that lie above the given suffix, in the
// order they appear in the name.
func labelsAbove(n, suffix Name) []string {
	labels := n.Labels()
	return labels[:len(labels)-suffix.LabelCount()]
}

// ReverseOwner returns the owner name a PTR for addr takes inside z.
//
// Usually that is simply the reverse name of the address, because a reverse
// zone on an octet or nibble boundary is an ancestor of every name below it.
// The exception is RFC 2317: a classless child such as
// "0/25.2.0.192.in-addr.arpa." is *not* an ancestor of "10.2.0.192.in-addr.arpa.",
// even though it answers for that address. The host part is re-attached under
// the child's own apex instead, giving "10.0/25.2.0.192.in-addr.arpa.", which
// is the name the parent zone's generated CNAME points at.
func (z Zone) ReverseOwner(addr netip.Addr) (Name, error) {
	if z.Kind != KindReverse {
		return Name{}, fmt.Errorf("%w: %q is not a reverse zone", ErrInvalid, z.Name)
	}
	if !z.Prefix.Contains(addr.Unmap()) {
		return Name{}, fmt.Errorf("%w: %v is outside %v, the network %q answers for",
			ErrInvalid, addr, z.Prefix, z.Name)
	}

	plain, err := ReverseName(addr)
	if err != nil {
		return Name{}, err
	}
	if plain.IsSubDomainOf(z.Name) {
		return plain, nil
	}

	// A classless zone sits one label below the zone the plain name belongs to,
	// so the labels the plain name has above that one are the host part.
	parent, ok := z.Name.Parent()
	if !ok || !plain.IsSubDomainOf(parent) {
		return Name{}, fmt.Errorf(
			"%w: %q answers for %v but is neither an ancestor of %q nor a classless child "+
				"of one (RFC 2317 §4)", ErrInvalid, z.Name, addr, plain)
	}

	host := labelsAbove(plain, parent)
	owner := z.Name
	for i := len(host) - 1; i >= 0; i-- {
		if owner, err = owner.Child(host[i]); err != nil {
			return Name{}, err
		}
	}
	return owner, nil
}
