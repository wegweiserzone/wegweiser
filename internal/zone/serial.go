package zone

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// serialSpace is the size of the serial number space, and serialHalf the point
// at which RFC 1982 stops being able to tell newer from older.
const (
	serialHalf = uint32(1) << 31
	// MaxSerialIncrement is the largest step RFC 1982 §3.1 defines for adding
	// to a serial. Adding more than this has no defined meaning, because the
	// result would not be recognisable as newer.
	MaxSerialIncrement = uint32(1<<31 - 1)
)

// Serial is a DNS zone serial number, with the arithmetic of RFC 1982.
//
// It is a struct rather than a bare uint32 so that "<" and ">" do not compile.
// Serials wrap: 4294967295 is older than 0, and comparing them as plain
// integers is the classic way to make a secondary refuse a transfer forever.
// Use [Serial.After], [Serial.Before] or [Serial.Compare]. Equality with "=="
// is fine and is exactly what RFC 1982 §3.2 defines it to be.
type Serial struct {
	v uint32
}

// NewSerial returns the serial with the given numeric value.
func NewSerial(v uint32) Serial { return Serial{v: v} }

// Uint32 returns the numeric value of the serial, for storage and the wire.
func (s Serial) Uint32() uint32 { return s.v }

// String returns the serial in decimal, as a zonefile writes it.
func (s Serial) String() string { return strconv.FormatUint(uint64(s.v), 10) }

// Next returns the following serial, wrapping past the end of the space.
//
// This is the only increment the journal uses: one commit advances a zone by
// exactly one, which is what lets IXFR replay commits directly. See
// docs/decisions/, D2.
func (s Serial) Next() Serial { return Serial{v: s.v + 1} }

// Add returns the serial n steps on, wrapping past the end of the space.
//
// RFC 1982 §3.1 defines addition only for n up to [MaxSerialIncrement]; adding
// more would produce a value that is not recognisably newer, so it is refused
// rather than silently wrapping into the past.
func (s Serial) Add(n uint32) (Serial, error) {
	if n > MaxSerialIncrement {
		return Serial{}, fmt.Errorf(
			"%w: cannot advance a serial by %d, the limit is %d (RFC 1982 §3.1)",
			ErrInvalid, n, MaxSerialIncrement)
	}
	return Serial{v: s.v + n}, nil
}

// Compare orders s against o by the rules of RFC 1982 §3.2, returning a
// negative number, zero, or a positive number as s is older than, the same as,
// or newer than o.
//
// When the two are exactly half the space apart, RFC 1982 leaves the relation
// undefined. Compare still answers, falling back to the raw numeric order so
// that it stays antisymmetric and callers get a stable answer rather than one
// that depends on argument order. [Serial.Comparable] reports that case, and a
// caller that must not guess should ask first.
func (s Serial) Compare(o Serial) int {
	if s.v == o.v {
		return 0
	}
	// Unsigned subtraction wraps, which is precisely the arithmetic wanted.
	switch diff := o.v - s.v; {
	case diff < serialHalf:
		return -1
	case diff > serialHalf:
		return 1
	default:
		if s.v < o.v {
			return -1
		}
		return 1
	}
}

// Comparable reports whether RFC 1982 §3.2 defines an ordering for s and o.
func (s Serial) Comparable(o Serial) bool {
	return o.v-s.v != serialHalf
}

// After reports whether s is newer than o.
func (s Serial) After(o Serial) bool { return s.Compare(o) > 0 }

// Before reports whether s is older than o.
func (s Serial) Before(o Serial) bool { return s.Compare(o) < 0 }

// IsZero reports whether s is the zero serial. Zero is a legal serial value,
// so this is only useful for spotting a field that was never set.
func (s Serial) IsZero() bool { return s.v == 0 }

// MarshalJSON implements [json.Marshaler], writing the serial as a number.
func (s Serial) MarshalJSON() ([]byte, error) { return []byte(s.String()), nil }

// UnmarshalJSON implements [json.Unmarshaler].
func (s *Serial) UnmarshalJSON(data []byte) error {
	var v uint32
	if err := json.Unmarshal(data, &v); err != nil {
		return fmt.Errorf("%w serial: %s", ErrInvalid, data)
	}
	s.v = v
	return nil
}
