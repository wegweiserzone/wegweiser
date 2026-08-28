package zone

import "fmt"

// TSIGAlgorithm names the keyed hash a TSIG key signs with, written the way
// RFC 8945 §4.2 puts it on the wire: in domain name syntax, with the trailing
// dot.
type TSIGAlgorithm string

// The algorithms this server offers.
//
// RFC 8945 §6 also makes hmac-sha1 MUST-implement and lists HMAC-MD5 as MAY.
// Neither is here: the same table calls hmac-sha1 NOT RECOMMENDED and forbids
// using HMAC-MD5, and offering an algorithm alongside advice not to use it is a
// footgun with a manual page. See docs/decisions/d28-tsig.md, which argues the
// departure.
const (
	HMACSHA256 TSIGAlgorithm = "hmac-sha256."
	HMACSHA384 TSIGAlgorithm = "hmac-sha384."
	HMACSHA512 TSIGAlgorithm = "hmac-sha512."
)

// TSIGAlgorithms are the ones this server signs and verifies with, in the order
// a client choosing between them should read.
func TSIGAlgorithms() []TSIGAlgorithm {
	return []TSIGAlgorithm{HMACSHA256, HMACSHA384, HMACSHA512}
}

// ParseTSIGAlgorithm reads an algorithm name, with or without its trailing dot
// and in any case: RFC 8945 §9 compares these as domain names.
func ParseTSIGAlgorithm(s string) (TSIGAlgorithm, error) {
	name, err := ParseName(s)
	if err != nil {
		return "", fmt.Errorf("%w: %q is not an algorithm name: %w", ErrInvalid, s, err)
	}
	a := TSIGAlgorithm(name.String())
	if !a.Valid() {
		return "", fmt.Errorf("%w: this server does not sign with %q; it offers %s",
			ErrInvalid, s, joinAlgorithms())
	}
	return a, nil
}

// Valid reports whether a is one this server will use.
func (a TSIGAlgorithm) Valid() bool {
	switch a {
	case HMACSHA256, HMACSHA384, HMACSHA512:
		return true
	default:
		return false
	}
}

// String returns the algorithm as it goes on the wire.
func (a TSIGAlgorithm) String() string { return string(a) }

// SecretBytes is how long a secret for this algorithm should be. RFC 8945 §8
// asks for at least the length of the keyed hash output, and there is no reason
// to generate less.
func (a TSIGAlgorithm) SecretBytes() int {
	switch a {
	case HMACSHA384:
		return 48
	case HMACSHA512:
		return 64
	default:
		return 32
	}
}

// joinAlgorithms lists what is on offer, for a refusal that says what to do
// instead.
func joinAlgorithms() string {
	var b []byte
	for i, a := range TSIGAlgorithms() {
		if i > 0 {
			b = append(b, ", "...)
		}
		b = append(b, a...)
	}
	return string(b)
}
