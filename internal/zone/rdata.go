package zone

import (
	"fmt"
	"net/netip"
	"reflect"
	"strings"

	"github.com/miekg/dns"
)

// RData is validated record data in canonical presentation format.
//
// Canonical means that two spellings of the same data produce identical bytes:
// whitespace is normalised, an IPv6 address is compressed, and a domain name
// inside the data is fully qualified and case-folded. Equality of RData is
// therefore string equality, which is what lets the database catch a duplicate
// record with a plain unique index. See docs/decisions/d18-rdata-presentation-format.md.
type RData struct {
	text string
}

// ErrInvalidRData reports record data that cannot be parsed or canonicalised.
var ErrInvalidRData = fmt.Errorf("%w record data", ErrInvalid)

// Two origins that no real zone can be under, used to detect a name in the
// data that is not fully qualified. Data that parses to the same thing under
// both is origin independent, which is exactly the property required.
const (
	probeOriginA = "origin-probe-a.invalid."
	probeOriginB = "origin-probe-b.invalid."
)

// ParseRData parses and canonicalises record data of the given type and class.
//
// Names inside the data must be fully qualified. Resolving a relative name
// needs an origin, which belongs to the zonefile parser rather than to the
// model, so a relative name is reported rather than silently attached to the
// wrong parent.
//
// The unknown-record form of RFC 3597 §5, "\# <length> <hex>", is accepted for
// any type. For a type we do know, it is converted to that type's own
// presentation form, as RFC 3597 §5 requires.
func ParseRData(t RRType, c Class, s string) (RData, error) {
	if !t.Storable() {
		return RData{}, fmt.Errorf("%w: %s records cannot be stored in a zone", ErrInvalidRData, t)
	}
	if !c.Storable() {
		return RData{}, fmt.Errorf("%w: class %s cannot be stored in a zone", ErrInvalidRData, c)
	}
	// Refused before parsing, because the parser accepts a record with no data
	// at all and silently zero-fills it: an A record with no address, a CAA
	// with an empty tag and value. Every such record would look plausible and
	// be wrong.
	if strings.TrimSpace(s) == "" {
		return RData{}, fmt.Errorf("%w: %s record has no data", ErrInvalidRData, t)
	}

	rr, text, err := parseRDataOnce(t, c, s, probeOriginA)
	if err != nil {
		return RData{}, err
	}

	// The origin can only reach the data through a name field, so the second
	// parse is only needed when there is one. That skips it for A, AAAA and
	// TXT, which is nearly every record.
	fields := domainNameFields(rr)
	if len(fields) > 0 {
		_, other, err := parseRDataOnce(t, c, s, probeOriginB)
		if err != nil {
			return RData{}, err
		}
		if other != text {
			return RData{}, fmt.Errorf(
				"%w: %q contains a name that is not fully qualified; add a trailing dot",
				ErrInvalidRData, s)
		}

		// Case-fold and re-escape every name through Name, rather than
		// lower-casing the text. Lower-casing would leave "\065" alone while
		// folding a literal "A", so the two spellings of one name would be
		// stored as two different records.
		for _, f := range fields {
			if f.String() == "" {
				return RData{}, fmt.Errorf("%w: %q is missing a name", ErrInvalidRData, s)
			}
			n, nerr := ParseName(f.String())
			if nerr != nil {
				return RData{}, fmt.Errorf("%w: %q in %q: %w", ErrInvalidRData, f.String(), s, nerr)
			}
			f.SetString(n.String())
		}
		if text, err = extractRData(rr); err != nil {
			return RData{}, err
		}
	}

	// Refuse to store anything we cannot read back. The canonical form is what
	// a zonefile export writes and what a re-import has to parse, so data that
	// does not survive that trip would quietly break the migration path.
	if _, reparsed, err := parseRDataOnce(t, c, text, probeOriginA); err != nil || reparsed != text {
		return RData{}, fmt.Errorf(
			"%w: %s data %q has no stable presentation form and cannot be stored",
			ErrInvalidRData, t, s)
	}

	return RData{text: text}, nil
}

// MustParseRData is [ParseRData] for data known to be valid at compile time,
// such as test fixtures. It panics if the data is not valid.
func MustParseRData(t RRType, c Class, s string) RData {
	r, err := ParseRData(t, c, s)
	if err != nil {
		panic("zone: MustParseRData: " + err.Error())
	}
	return r
}

// parseRDataOnce parses the data once under the given origin, returning the
// parsed record and its canonical data text.
func parseRDataOnce(t RRType, c Class, s, origin string) (dns.RR, string, error) {
	// An absolute owner name keeps the origin out of the header, so only the
	// data can differ between the two probe parses.
	line := "probe. 0 " + c.String() + " " + t.String() + " " + s

	zp := dns.NewZoneParser(strings.NewReader(line), origin, "")
	rr, ok := zp.Next()
	if !ok {
		if err := zp.Err(); err != nil {
			return nil, "", fmt.Errorf("%w: %s", ErrInvalidRData, cleanParserError(err))
		}
		return nil, "", fmt.Errorf("%w: empty", ErrInvalidRData)
	}
	// Data spanning several records would mean the input smuggled a newline
	// and a second record past us.
	if _, more := zp.Next(); more {
		return nil, "", fmt.Errorf("%w: %q describes more than one record", ErrInvalidRData, s)
	}
	if err := zp.Err(); err != nil {
		return nil, "", fmt.Errorf("%w: %s", ErrInvalidRData, cleanParserError(err))
	}

	text, err := extractRData(rr)
	if err != nil {
		return nil, "", err
	}
	return rr, text, nil
}

// RDataFromRR takes the data out of a record the wire library parsed.
//
// It is the inverse of the trip [ParseRData] makes, and it exists for the
// zonefile reader: that reader hands whole files to the library's own RFC 1035
// §5 parser rather than re-deriving the format, and needs the records it gets
// back in the canonical presentation form D18 asks for.
//
// The parameter is the wire library's own type, which widens the dependency
// D22 admits from an internal one to an exported signature. Deliberate,
// and small: the alternative is a caller re-implementing the split below, and a
// second answer to "where does the header end" is exactly the drift the
// canonical form exists to prevent.
func RDataFromRR(rr dns.RR) (RData, error) {
	text, err := extractRData(rr)
	if err != nil {
		return RData{}, err
	}
	return ParseRData(RRType(rr.Header().Rrtype), Class(rr.Header().Class), text)
}

// extractRData returns the data portion of a record's presentation form.
func extractRData(rr dns.RR) (string, error) {
	const headerFields = 4

	parts := strings.SplitN(rr.String(), "\t", headerFields+1)
	if len(parts) <= headerFields {
		return "", fmt.Errorf("%w: %s record has no data", ErrInvalidRData,
			RRType(rr.Header().Rrtype))
	}
	// The zero-length unknown-record form prints as "\# 0 " with a trailing
	// space, which would otherwise make it differ from its own reparse.
	text := strings.TrimSpace(parts[headerFields])
	if text == "" {
		// Reachable when the parser accepted a field it could not fill, such
		// as an address that was never given.
		return "", fmt.Errorf("%w: %s record has no data", ErrInvalidRData,
			RRType(rr.Header().Rrtype))
	}
	return text, nil
}

// domainNameFields returns the settable string fields of rr that hold a domain
// name, identified by the struct tags the wire library uses for them.
func domainNameFields(rr dns.RR) []reflect.Value {
	v := reflect.ValueOf(rr)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return nil
	}
	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return nil
	}

	var fields []reflect.Value
	t := v.Type()
	for i := range t.NumField() {
		if !strings.Contains(t.Field(i).Tag.Get("dns"), "domain-name") {
			continue
		}
		f := v.Field(i)
		if f.Kind() == reflect.String && f.CanSet() {
			fields = append(fields, f)
		}
	}
	return fields
}

// cleanParserError strips the synthetic line and column the probe parse adds,
// which refer to a line the caller never wrote.
func cleanParserError(err error) string {
	msg := strings.TrimPrefix(err.Error(), "dns: ")
	if i := strings.LastIndex(msg, " at line: "); i >= 0 {
		msg = msg[:i]
	}
	return msg
}

// RDataFromCanonical wraps text that [ParseRData] has already produced.
func RDataFromCanonical(text string) (RData, error) {
	if text == "" {
		return RData{}, fmt.Errorf("%w: there is none", ErrInvalidRData)
	}
	return RData{text: text}, nil
}

// String returns the canonical presentation form of the data.
func (r RData) String() string { return r.text }

// IsZero reports whether r is the zero value, meaning no data at all.
func (r RData) IsZero() bool { return r.text == "" }

// Equal reports whether r and o hold the same data. Because both are canonical,
// this is an exact comparison rather than a semantic one.
func (r RData) Equal(o RData) bool { return r.text == o.text }

// Address returns the IP address carried by A and AAAA data, and reports false
// for every other type.
func (r RData) Address(t RRType) (netip.Addr, bool) {
	if t != TypeA && t != TypeAAAA {
		return netip.Addr{}, false
	}
	addr, err := netip.ParseAddr(r.text)
	if err != nil {
		return netip.Addr{}, false
	}
	if (t == TypeA) != addr.Is4() {
		return netip.Addr{}, false
	}
	return addr, true
}

// MarshalText implements [encoding.TextMarshaler].
func (r RData) MarshalText() ([]byte, error) { return []byte(r.text), nil }
