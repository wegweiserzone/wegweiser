package zone

import (
	"fmt"
	"strings"
)

// Size limits from RFC 1035 §2.3.4.
const (
	// MaxLabelLen is the largest permitted label, in octets.
	MaxLabelLen = 63
	// MaxNameWireLen is the largest permitted encoded name, in octets. It
	// counts the length octet of every label and the terminating zero octet,
	// which is why the printable form of a maximal name is shorter than this.
	MaxNameWireLen = 255
	// maxLabels is the most labels a valid name can hold. The shortest label
	// encoding is two octets, one for the length and one for the content, so
	// no name reaches this bound.
	maxLabels = MaxNameWireLen / 2
)

// Errors returned when a name cannot be parsed. They are joined to a message
// naming the offending input, so callers should test with [errors.Is] rather
// than by comparing strings.
var (
	// ErrInvalidName reports a name that is malformed for any reason. Every
	// other error in this group wraps it, so a caller that does not care which
	// rule was broken can test for this one alone.
	ErrInvalidName = fmt.Errorf("%w domain name", ErrInvalid)
	// ErrNameTooLong reports a name whose encoded form exceeds
	// [MaxNameWireLen].
	ErrNameTooLong = fmt.Errorf("%w: longer than %d octets", ErrInvalidName, MaxNameWireLen)
	// ErrLabelTooLong reports a label longer than [MaxLabelLen].
	ErrLabelTooLong = fmt.Errorf("%w: label longer than %d octets", ErrInvalidName, MaxLabelLen)
	// ErrEmptyLabel reports an empty label, as produced by a leading dot or by
	// two consecutive dots.
	ErrEmptyLabel = fmt.Errorf("%w: empty label", ErrInvalidName)
	// ErrBadEscape reports a backslash escape that is not a single character
	// or exactly three decimal digits in the range 0 to 255 (RFC 1035 §5.1).
	ErrBadEscape = fmt.Errorf("%w: malformed escape", ErrInvalidName)
)

// Name is a fully qualified, validated domain name.
//
// The value is held in uncompressed wire form (every label preceded by its
// length octet, the whole terminated by a zero octet) with US-ASCII letters
// lowercased. Two names are equal exactly when their encoded forms are equal,
// which makes Name comparable, usable as a map key, and correct with respect to
// the case-insensitive comparison required by RFC 4343.
//
// Lowercasing on the way in is a storage decision, not a wire decision. A
// response echoes the casing of the query's QNAME (0x20 encoding), never the
// stored casing, so nothing observable on the wire depends on it. It also gives
// DNSSEC the canonical form it will need later, free (RFC 4034 §6.2).
type Name struct {
	wire string
}

// Root is the DNS root, ".". Every name is a subdomain of it.
var Root = Name{wire: "\x00"}

// ParseName parses a domain name in the presentation format of RFC 1035 §5.1.
func ParseName(s string) (Name, error) {
	if s == "" {
		return Name{}, fmt.Errorf("%w: empty", ErrInvalidName)
	}
	if s == "." {
		return Root, nil
	}

	// The encoded form is at most one octet longer than the input, for the
	// terminating zero, and escapes only ever shrink it.
	wire := make([]byte, 0, len(s)+1)
	label := make([]byte, 0, MaxLabelLen)

	flush := func() error {
		n := len(label)
		if n == 0 {
			return fmt.Errorf("%w in %q", ErrEmptyLabel, s)
		}
		if n > MaxLabelLen {
			return fmt.Errorf("%w: %q in %q", ErrLabelTooLong, label, s)
		}
		wire = append(wire, byte(n))
		wire = append(wire, label...)
		label = label[:0]
		return nil
	}

	for i := 0; i < len(s); {
		switch c := s[i]; c {
		case '.':
			i++
			if err := flush(); err != nil {
				return Name{}, err
			}

		case '\\':
			b, n, err := decodeEscape(s[i:])
			if err != nil {
				return Name{}, fmt.Errorf("%w in %q", err, s)
			}
			label = append(label, lowerASCII(b))
			i += n

		default:
			label = append(label, lowerASCII(c))
			i++
		}
	}

	// A name that does not end in a dot has one label still pending. A name
	// that does has already flushed it, and label is empty.
	if len(label) > 0 {
		if err := flush(); err != nil {
			return Name{}, err
		}
	}

	wire = append(wire, 0)
	if len(wire) > MaxNameWireLen {
		return Name{}, fmt.Errorf("%w: %q", ErrNameTooLong, s)
	}
	return Name{wire: string(wire)}, nil
}

// MustParseName is [ParseName] for names known to be valid at compile time,
// such as constants and test fixtures. It panics if s is not a valid name.
func MustParseName(s string) Name {
	n, err := ParseName(s)
	if err != nil {
		panic("zone: MustParseName: " + err.Error())
	}
	return n
}

// NameFromWire parses an uncompressed wire-format name.
//
// Compression pointers are rejected: a Name is a standalone value, and a
// pointer is only meaningful relative to the message it was read from. The
// caller, the query path, resolves pointers while parsing the message and
// passes the expanded name here.
func NameFromWire(b []byte) (Name, error) {
	if len(b) == 0 {
		return Name{}, fmt.Errorf("%w: empty wire name", ErrInvalidName)
	}
	if len(b) > MaxNameWireLen {
		return Name{}, ErrNameTooLong
	}

	wire := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		n := int(b[i])
		switch {
		case n == 0:
			if i != len(b)-1 {
				return Name{}, fmt.Errorf("%w: %d trailing octets after the root label",
					ErrInvalidName, len(b)-i-1)
			}
			wire = append(wire, 0)
			return Name{wire: string(wire)}, nil

		case n&0xC0 != 0:
			// 0xC0 marks a compression pointer (RFC 1035 §4.1.4); 0x40 and
			// 0x80 are reserved label types that were never assigned.
			return Name{}, fmt.Errorf("%w: label type 0x%02x is not a plain label",
				ErrInvalidName, n&0xC0)

		case n > MaxLabelLen:
			return Name{}, ErrLabelTooLong

		case i+1+n > len(b):
			return Name{}, fmt.Errorf("%w: label of %d octets overruns the buffer",
				ErrInvalidName, n)
		}

		wire = append(wire, byte(n))
		for _, c := range b[i+1 : i+1+n] {
			wire = append(wire, lowerASCII(c))
		}
		i += 1 + n
	}
	return Name{}, fmt.Errorf("%w: not terminated by a root label", ErrInvalidName)
}

// String returns the name in presentation format, fully qualified with a
// trailing dot.
//
// Octets that are not printable US-ASCII, and those that would otherwise be
// read as zonefile syntax, are escaped, so the result is always safe to write
// into a zonefile and always parses back to an equal Name.
func (n Name) String() string {
	if n.IsZero() {
		return ""
	}
	if n.wire == "\x00" {
		return "."
	}

	var b strings.Builder
	b.Grow(len(n.wire) + 8)
	for i := 0; i < len(n.wire); {
		l := int(n.wire[i])
		if l == 0 {
			break
		}
		for _, c := range []byte(n.wire[i+1 : i+1+l]) {
			writeEscaped(&b, c)
		}
		b.WriteByte('.')
		i += 1 + l
	}
	return b.String()
}

// IsZero reports whether n is the zero value, meaning no name at all. It is
// distinct from the root, which is a real name.
func (n Name) IsZero() bool { return n.wire == "" }

// IsRoot reports whether n is the root name, ".".
func (n Name) IsRoot() bool { return n.wire == "\x00" }

// Equal reports whether n and m are the same name. Comparison is
// case-insensitive for US-ASCII letters, as required by RFC 4343, because both
// names were lowercased when they were parsed.
func (n Name) Equal(m Name) bool { return n.wire == m.wire }

// WireLen returns the length of the encoded name in octets, including every
// length octet and the terminating zero.
func (n Name) WireLen() int { return len(n.wire) }

// AppendWire appends the uncompressed wire form of n to dst and returns the
// extended buffer. It does not allocate when dst has spare capacity.
func (n Name) AppendWire(dst []byte) []byte { return append(dst, n.wire...) }

// Wire returns a copy of the uncompressed wire form of n.
func (n Name) Wire() []byte { return []byte(n.wire) }

// LabelCount returns the number of labels in n, excluding the root label. The
// root itself has zero labels.
func (n Name) LabelCount() int {
	count := 0
	for i := 0; i < len(n.wire); {
		l := int(n.wire[i])
		if l == 0 {
			break
		}
		count++
		i += 1 + l
	}
	return count
}

// Labels returns the labels of n from left to right, as raw octets without
// escaping. The root has no labels.
func (n Name) Labels() []string {
	labels := make([]string, 0, n.LabelCount())
	for i := 0; i < len(n.wire); {
		l := int(n.wire[i])
		if l == 0 {
			break
		}
		labels = append(labels, n.wire[i+1:i+1+l])
		i += 1 + l
	}
	return labels
}

// FirstLabel returns the leftmost label of n as raw octets, without escaping,
// and reports false for the root and for the zero Name.
func (n Name) FirstLabel() (string, bool) {
	if n.IsZero() || n.IsRoot() {
		return "", false
	}
	l := int(n.wire[0])
	return n.wire[1 : 1+l], true
}

// Parent returns the name one label up, reporting false for the root and for
// the zero Name. The parent of "www.example.com." is "example.com.", and the
// parent of "com." is the root.
func (n Name) Parent() (Name, bool) {
	if n.IsZero() || n.IsRoot() {
		return Name{}, false
	}
	l := int(n.wire[0])
	return Name{wire: n.wire[1+l:]}, true
}

// Child returns n with label prepended, so Child("www") on "example.com."
// yields "www.example.com.".
func (n Name) Child(label string) (Name, error) {
	if n.IsZero() {
		return Name{}, fmt.Errorf("%w: no parent name", ErrInvalidName)
	}
	l := len(label)
	if l == 0 {
		return Name{}, ErrEmptyLabel
	}
	if l > MaxLabelLen {
		return Name{}, fmt.Errorf("%w: %q", ErrLabelTooLong, label)
	}
	if 1+l+len(n.wire) > MaxNameWireLen {
		return Name{}, fmt.Errorf("%w: %q under %q", ErrNameTooLong, label, n)
	}

	b := make([]byte, 0, 1+l+len(n.wire))
	b = append(b, byte(l))
	for i := range l {
		b = append(b, lowerASCII(label[i]))
	}
	b = append(b, n.wire...)
	return Name{wire: string(b)}, nil
}

// IsSubDomainOf reports whether n lies at or below p, so a name is a subdomain
// of itself and of the root. This matches how RFC 1034 §4.3.2 uses the term
// when deciding whether a query falls inside a zone.
func (n Name) IsSubDomainOf(p Name) bool {
	if n.IsZero() || p.IsZero() || len(p.wire) > len(n.wire) {
		return false
	}
	// Suffix matching has to respect label boundaries: "notexample.com."
	// ends with the octets of "example.com." but is not below it.
	for i := 0; i < len(n.wire); {
		if len(n.wire)-i == len(p.wire) {
			return n.wire[i:] == p.wire
		}
		l := int(n.wire[i])
		if l == 0 {
			return false
		}
		i += 1 + l
	}
	return false
}

// Compare orders n against m in the canonical name order of RFC 4034 §6.1:
// labels are compared from the rightmost, as unsigned octets, with a label
// that is a prefix of another sorting first. It returns a negative number, 0,
// or a positive number as n sorts before, equal to, or after m.
func (n Name) Compare(m Name) int {
	var abuf, bbuf [maxLabels]int
	a := labelOffsets(n.wire, abuf[:0])
	b := labelOffsets(m.wire, bbuf[:0])

	i, j := len(a)-1, len(b)-1
	for i >= 0 && j >= 0 {
		if c := strings.Compare(labelAt(n.wire, a[i]), labelAt(m.wire, b[j])); c != 0 {
			return c
		}
		i--
		j--
	}
	switch {
	case i >= 0:
		return 1 // n has ancestors left over, so it lies below m
	case j >= 0:
		return -1
	default:
		return 0
	}
}

// SortKey returns a byte string whose plain byte order equals the canonical
// name order of [Compare]. It is stored alongside the name so that a database
// ORDER BY reproduces DNS ordering without a custom collation.
//
// The encoding reverses the labels and terminates each with two zero octets. A
// zero octet inside a label (legal, if vanishingly rare) is escaped to 0x00
// 0xFF, which keeps the encoding unambiguous and keeps a terminator sorting
// before any label content, exactly as a shorter name must sort before the
// names below it.
func (n Name) SortKey() []byte {
	var buf [maxLabels]int
	offs := labelOffsets(n.wire, buf[:0])

	key := make([]byte, 0, len(n.wire)+2*len(offs))
	for i := len(offs) - 1; i >= 0; i-- {
		for _, c := range []byte(labelAt(n.wire, offs[i])) {
			key = append(key, c)
			if c == 0x00 {
				key = append(key, 0xFF)
			}
		}
		key = append(key, 0x00, 0x00)
	}
	return key
}

// MarshalText implements [encoding.TextMarshaler], so a Name renders as its
// presentation form in JSON and YAML alike.
func (n Name) MarshalText() ([]byte, error) { return []byte(n.String()), nil }

// UnmarshalText implements [encoding.TextUnmarshaler]. Decoding validates the
// name, so an invalid name cannot enter the model through an API request.
func (n *Name) UnmarshalText(text []byte) error {
	parsed, err := ParseName(string(text))
	if err != nil {
		return err
	}
	*n = parsed
	return nil
}

// labelOffsets appends the offset of each label's length octet in wire to dst.
func labelOffsets(wire string, dst []int) []int {
	for i := 0; i < len(wire); {
		l := int(wire[i])
		if l == 0 {
			break
		}
		dst = append(dst, i)
		i += 1 + l
	}
	return dst
}

// labelAt returns the label content at the given length-octet offset.
func labelAt(wire string, off int) string {
	return wire[off+1 : off+1+int(wire[off])]
}

// decodeEscape decodes the backslash escape at the start of s, returning the
// octet it denotes and how many input octets it consumed (RFC 1035 §5.1).
func decodeEscape(s string) (b byte, n int, err error) {
	if len(s) < 2 {
		return 0, 0, fmt.Errorf("%w: trailing backslash", ErrBadEscape)
	}
	if !isDigit(s[1]) {
		return s[1], 2, nil
	}
	if len(s) < 4 || !isDigit(s[2]) || !isDigit(s[3]) {
		return 0, 0, fmt.Errorf("%w: %q needs exactly three digits", ErrBadEscape, s[:min(len(s), 4)])
	}
	// Unsigned arithmetic keeps the value provably non-negative, so the only
	// bound that has to be checked is the upper one.
	v := uint(s[1]-'0')*100 + uint(s[2]-'0')*10 + uint(s[3]-'0')
	if v > 255 {
		return 0, 0, fmt.Errorf("%w: %q exceeds 255", ErrBadEscape, s[:4])
	}
	return byte(v), 4, nil
}

// writeEscaped writes one octet of a label in presentation format, escaping it
// where a literal would be unreadable or would be misread as zonefile syntax.
func writeEscaped(b *strings.Builder, c byte) {
	switch {
	case isZonefileSpecial(c):
		b.WriteByte('\\')
		b.WriteByte(c)
	case c < 0x21 || c > 0x7E:
		// Not printable US-ASCII, so use the numeric form.
		b.WriteByte('\\')
		b.WriteByte('0' + c/100)
		b.WriteByte('0' + (c/10)%10)
		b.WriteByte('0' + c%10)
	default:
		b.WriteByte(c)
	}
}

// isZonefileSpecial reports whether an octet has a syntactic meaning in a
// zonefile and therefore has to be escaped inside a label.
func isZonefileSpecial(c byte) bool {
	switch c {
	case ' ', '"', '\'', '(', ')', '.', ';', '@', '\\':
		return true
	default:
		return false
	}
}

// lowerASCII lowercases a US-ASCII letter and leaves every other octet alone.
// Only A to Z are affected, as required by RFC 4343: an octet above 0x7F is
// part of no known alphabet as far as DNS is concerned.
func lowerASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// isDigit reports whether c is a US-ASCII decimal digit.
func isDigit(c byte) bool { return c >= '0' && c <= '9' }
