package zone

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/miekg/dns"
)

// RRType is a resource record type (RFC 1035 §3.2.2).
//
// The numeric value is the IANA assignment, so an unknown type carries through
// the system unchanged and round-trips as required by RFC 3597.
type RRType uint16

// The record types the zone logic itself reasons about, plus the ones an
// operator meets day to day. Any other type is still valid and is handled by
// number; these exist so the code can name what it means. Values come from the
// IANA registry by way of the wire library, so there are no magic numbers here.
const (
	TypeNone  = RRType(dns.TypeNone)
	TypeA     = RRType(dns.TypeA)
	TypeNS    = RRType(dns.TypeNS)
	TypeCNAME = RRType(dns.TypeCNAME)
	TypeSOA   = RRType(dns.TypeSOA)
	TypeNULL  = RRType(dns.TypeNULL)
	TypePTR   = RRType(dns.TypePTR)
	TypeHINFO = RRType(dns.TypeHINFO)
	TypeMX    = RRType(dns.TypeMX)
	TypeTXT   = RRType(dns.TypeTXT)
	TypeAAAA  = RRType(dns.TypeAAAA)
	TypeSRV   = RRType(dns.TypeSRV)
	TypeNAPTR = RRType(dns.TypeNAPTR)
	TypeDNAME = RRType(dns.TypeDNAME)
	TypeSVCB  = RRType(dns.TypeSVCB)
	TypeHTTPS = RRType(dns.TypeHTTPS)
	TypeCAA   = RRType(dns.TypeCAA)
	TypeTLSA  = RRType(dns.TypeTLSA)
	TypeSSHFP = RRType(dns.TypeSSHFP)

	// DNSSEC types. Out of scope for v0.1, named so validation can recognise
	// and reject them rather than storing records it cannot maintain.
	TypeDS         = RRType(dns.TypeDS)
	TypeRRSIG      = RRType(dns.TypeRRSIG)
	TypeNSEC       = RRType(dns.TypeNSEC)
	TypeDNSKEY     = RRType(dns.TypeDNSKEY)
	TypeNSEC3      = RRType(dns.TypeNSEC3)
	TypeNSEC3PARAM = RRType(dns.TypeNSEC3PARAM)
	// SIG, KEY and NXT are the RFC 2535 originals that RFC 3755 replaced with
	// RRSIG, DNSKEY and NSEC. They belong to the same family.
	TypeSIG = RRType(dns.TypeSIG)
	TypeKEY = RRType(dns.TypeKEY)
	TypeNXT = RRType(dns.TypeNXT)

	// Meta types, which exist only for the lifetime of one message.
	TypeOPT  = RRType(dns.TypeOPT)
	TypeTKEY = RRType(dns.TypeTKEY)
	TypeTSIG = RRType(dns.TypeTSIG)

	// Query types, which are only meaningful in a question.
	TypeIXFR  = RRType(dns.TypeIXFR)
	TypeAXFR  = RRType(dns.TypeAXFR)
	TypeMAILB = RRType(dns.TypeMAILB)
	TypeMAILA = RRType(dns.TypeMAILA)
	TypeANY   = RRType(dns.TypeANY)
)

// ErrInvalidRRType reports a record type that is neither a known mnemonic nor
// the TYPE<number> form of RFC 3597 §5.
var ErrInvalidRRType = fmt.Errorf("%w record type", ErrInvalid)

// String returns the mnemonic for t, or the TYPE<number> form of RFC 3597 §5
// when the type has no assigned mnemonic.
func (t RRType) String() string {
	if s, ok := dns.TypeToString[uint16(t)]; ok && isMnemonic(s) {
		return s
	}
	return "TYPE" + strconv.FormatUint(uint64(t), 10)
}

// HasMnemonic reports whether t is a type with an assigned mnemonic, rather
// than one that can only be written in the TYPE<number> form of RFC 3597 §5.
func (t RRType) HasMnemonic() bool {
	s, ok := dns.TypeToString[uint16(t)]
	return ok && isMnemonic(s)
}

// ParseRRType parses a record type given either as a mnemonic, in any casing,
// or in the TYPE<number> form of RFC 3597 §5.
func ParseRRType(s string) (RRType, error) {
	up := strings.ToUpper(strings.TrimSpace(s))
	if up == "" {
		return TypeNone, fmt.Errorf("%w: empty", ErrInvalidRRType)
	}
	if t, ok := dns.StringToType[up]; ok && isMnemonic(up) {
		return RRType(t), nil
	}
	if rest, ok := strings.CutPrefix(up, "TYPE"); ok {
		if n, err := strconv.ParseUint(rest, 10, 16); err == nil {
			return RRType(n), nil
		}
	}
	return TypeNone, fmt.Errorf("%w: %q", ErrInvalidRRType, s)
}

// IsQueryOnly reports whether t may appear only in a question and never in a
// zone: AXFR, IXFR, MAILA, MAILB and ANY are QTYPEs (RFC 6895 §3.1).
func (t RRType) IsQueryOnly() bool {
	switch t {
	case TypeIXFR, TypeAXFR, TypeMAILB, TypeMAILA, TypeANY:
		return true
	default:
		return false
	}
}

// IsMeta reports whether t is a meta type, carrying data that belongs to one
// message rather than to a zone: OPT (RFC 6891 §6.1.1), TSIG and TKEY
// (RFC 6895 §3.1).
func (t RRType) IsMeta() bool {
	switch t {
	case TypeOPT, TypeTKEY, TypeTSIG:
		return true
	default:
		return false
	}
}

// IsDNSSEC reports whether t is part of the DNSSEC record set. Wegweiser does
// not sign zones yet, and a hand-written signature record would be actively
// harmful, so validation refuses these until signing exists.
func (t RRType) IsDNSSEC() bool {
	switch t {
	case TypeDS, TypeRRSIG, TypeNSEC, TypeDNSKEY, TypeNSEC3, TypeNSEC3PARAM,
		TypeSIG, TypeKEY, TypeNXT:
		return true
	default:
		return false
	}
}

// Storable reports whether a record of type t may be held in a zone.
//
// Type 0 is reserved, query and meta types belong to messages rather than to
// zones, and RFC 1035 §3.3.10 states outright that NULL records are not allowed
// in zone files.
func (t RRType) Storable() bool {
	return t != TypeNone && t != TypeNULL && !t.IsQueryOnly() && !t.IsMeta()
}

// MarshalText implements [encoding.TextMarshaler], so a type appears as "AAAA"
// rather than as 28 in JSON and YAML.
func (t RRType) MarshalText() ([]byte, error) { return []byte(t.String()), nil }

// UnmarshalText implements [encoding.TextUnmarshaler].
func (t *RRType) UnmarshalText(text []byte) error {
	parsed, err := ParseRRType(string(text))
	if err != nil {
		return err
	}
	*t = parsed
	return nil
}

// Class is a resource record class (RFC 1035 §3.2.4). In practice everything
// is [ClassIN]; the others exist because the protocol has them.
type Class uint16

// The assigned classes.
const (
	ClassIN   = Class(dns.ClassINET)
	ClassCH   = Class(dns.ClassCHAOS)
	ClassHS   = Class(dns.ClassHESIOD)
	ClassNONE = Class(dns.ClassNONE)
	ClassANY  = Class(dns.ClassANY)
)

// ErrInvalidClass reports a class that is neither a known mnemonic nor the
// CLASS<number> form of RFC 3597 §5.
var ErrInvalidClass = fmt.Errorf("%w class", ErrInvalid)

// String returns the mnemonic for c, or the CLASS<number> form of RFC 3597 §5
// when the class has no assigned mnemonic.
func (c Class) String() string {
	if s, ok := dns.ClassToString[uint16(c)]; ok && isMnemonic(s) {
		return s
	}
	return "CLASS" + strconv.FormatUint(uint64(c), 10)
}

// ParseClass parses a class given either as a mnemonic, in any casing, or in
// the CLASS<number> form of RFC 3597 §5.
func ParseClass(s string) (Class, error) {
	up := strings.ToUpper(strings.TrimSpace(s))
	if up == "" {
		return 0, fmt.Errorf("%w: empty", ErrInvalidClass)
	}
	if c, ok := dns.StringToClass[up]; ok && isMnemonic(up) {
		return Class(c), nil
	}
	if rest, ok := strings.CutPrefix(up, "CLASS"); ok {
		if n, err := strconv.ParseUint(rest, 10, 16); err == nil {
			return Class(n), nil
		}
	}
	return 0, fmt.Errorf("%w: %q", ErrInvalidClass, s)
}

// Storable reports whether records of class c may be held in a zone. NONE and
// ANY are QCLASSes, meaningful only inside a message (RFC 6895 §3.2).
func (c Class) Storable() bool {
	return c != 0 && c != ClassNONE && c != ClassANY
}

// MarshalText implements [encoding.TextMarshaler].
func (c Class) MarshalText() ([]byte, error) { return []byte(c.String()), nil }

// UnmarshalText implements [encoding.TextUnmarshaler].
func (c *Class) UnmarshalText(text []byte) error {
	parsed, err := ParseClass(string(text))
	if err != nil {
		return err
	}
	*c = parsed
	return nil
}

// isMnemonic reports whether s is shaped like an RR type or class mnemonic:
// upper-case letters, digits and hyphens only.
func isMnemonic(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-':
		default:
			return false
		}
	}
	return true
}
