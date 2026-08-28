package zone

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// TTL is a resource record time to live, in seconds.
//
// It is seconds rather than a [time.Duration] because DNS TTLs are integral
// seconds on the wire and in a zonefile. A Duration would admit values such as
// 1500ms that have no representation, and would have to be rounded somewhere —
// silently, and in more than one place.
type TTL uint32

// MaxTTL is the largest permitted TTL.
//
// RFC 2181 §8 defines the TTL field as an unsigned 32-bit number whose top bit
// must be zero, and requires a receiver to treat a value with that bit set as
// zero. Accepting such a value on input would therefore store something that
// resolvers read as "do not cache", so it is refused instead.
const MaxTTL TTL = 1<<31 - 1

// ErrInvalidTTL reports a TTL that is malformed or out of range.
var ErrInvalidTTL = fmt.Errorf("%w TTL", ErrInvalid)

// ParseTTL parses a TTL given either as a plain number of seconds or in the
// suffixed form BIND uses, so a value pasted out of an existing zonefile is
// accepted as typed.
func ParseTTL(s string) (TTL, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return 0, fmt.Errorf("%w: empty", ErrInvalidTTL)
	}

	// The common case: a bare number of seconds.
	if n, err := strconv.ParseUint(trimmed, 10, 64); err == nil {
		if n > uint64(MaxTTL) {
			return 0, fmt.Errorf("%w: %s exceeds the maximum of %d", ErrInvalidTTL, trimmed, MaxTTL)
		}
		return TTL(n), nil
	}

	var (
		total    uint64
		seen     uint8 // bit set of the units already used
		start    int   // where the current run of digits began
		sections int
	)
	for i := range len(trimmed) {
		c := trimmed[i]
		if c >= '0' && c <= '9' {
			continue
		}

		mult, bit, ok := ttlUnit(c)
		if !ok {
			return 0, fmt.Errorf("%w: %q contains %q, which is not a unit; use s, m, h, d or w",
				ErrInvalidTTL, trimmed, string(c))
		}
		if i == start {
			return 0, fmt.Errorf("%w: %q has the unit %q without a number",
				ErrInvalidTTL, trimmed, string(c))
		}
		if seen&bit != 0 {
			return 0, fmt.Errorf("%w: %q repeats the unit %q", ErrInvalidTTL, trimmed, string(c))
		}
		seen |= bit

		n, err := strconv.ParseUint(trimmed[start:i], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: %q is out of range", ErrInvalidTTL, trimmed)
		}
		// Checked before multiplying, so the product cannot wrap.
		if n > uint64(MaxTTL)/mult {
			return 0, fmt.Errorf("%w: %s exceeds the maximum of %d", ErrInvalidTTL, trimmed, MaxTTL)
		}
		total += n * mult
		if total > uint64(MaxTTL) {
			return 0, fmt.Errorf("%w: %s exceeds the maximum of %d", ErrInvalidTTL, trimmed, MaxTTL)
		}
		start = i + 1
		sections++
	}

	if start != len(trimmed) {
		return 0, fmt.Errorf("%w: %q ends with a number that has no unit", ErrInvalidTTL, trimmed)
	}
	if sections == 0 {
		return 0, fmt.Errorf("%w: %q", ErrInvalidTTL, trimmed)
	}
	return TTL(total), nil
}

// ttlUnit maps a suffix character to its multiplier in seconds and to the bit
// that records having seen it.
func ttlUnit(c byte) (mult uint64, bit uint8, ok bool) {
	switch c {
	case 's', 'S':
		return 1, 1 << 0, true
	case 'm', 'M':
		return 60, 1 << 1, true
	case 'h', 'H':
		return 3600, 1 << 2, true
	case 'd', 'D':
		return 86400, 1 << 3, true
	case 'w', 'W':
		return 604800, 1 << 4, true
	default:
		return 0, 0, false
	}
}

// String returns the TTL as a plain number of seconds, which is the form a
// zonefile and the wire both use.
func (t TTL) String() string { return strconv.FormatUint(uint64(t), 10) }

// Duration returns the TTL as a [time.Duration], for arithmetic against clocks
// and timers.
func (t TTL) Duration() time.Duration { return time.Duration(t) * time.Second }

// Valid reports whether t is within the range RFC 2181 §8 permits.
func (t TTL) Valid() bool { return t <= MaxTTL }

// MarshalJSON implements [json.Marshaler], writing the TTL as a number so that
// a client receives 3600 rather than "3600".
func (t TTL) MarshalJSON() ([]byte, error) { return []byte(t.String()), nil }

// UnmarshalJSON implements [json.Unmarshaler]. It accepts both a number of
// seconds and a suffixed string such as "1h", so a hand-written YAML file can
// use whichever is clearer.
func (t *TTL) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		parsed, err := ParseTTL(s)
		if err != nil {
			return err
		}
		*t = parsed
		return nil
	}

	var n uint64
	if err := json.Unmarshal(data, &n); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidTTL, data)
	}
	if n > uint64(MaxTTL) {
		return fmt.Errorf("%w: %d exceeds the maximum of %d", ErrInvalidTTL, n, MaxTTL)
	}
	*t = TTL(n)
	return nil
}
