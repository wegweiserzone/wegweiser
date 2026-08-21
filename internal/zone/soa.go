package zone

import (
	"fmt"
	"strconv"
	"strings"
)

// SOA holds the start-of-authority parameters of a zone (RFC 1035 §3.3.13).
//
// It is modelled as fields on the zone rather than as an ordinary record. The
// serial belongs to the journal, not to the user: one commit advances it by
// exactly one, and that is the invariant IXFR replay depends on. As an editable
// record it would be one careless edit away from breaking every secondary. See
// data model §4.1.
type SOA struct {
	// NS is the MNAME field: the primary server for the zone.
	NS Name
	// Mbox is the RNAME field: the zone administrator's mailbox, written as a
	// domain name, so hostmaster@example.com is hostmaster.example.com.
	Mbox Name

	Serial Serial

	// Refresh, Retry and Expire pace a secondary's transfers. They are 32-bit
	// second counts on the wire, the same domain as a TTL, so they share the
	// type; the field names carry the meaning that the type does not.
	Refresh TTL
	Retry   TTL
	Expire  TTL
	// Minimum is the negative-caching TTL (RFC 2308 §4), not a floor on record
	// TTLs, despite what the name suggests.
	Minimum TTL

	// TTL is the time to live of the SOA record itself.
	TTL TTL
}

// DefaultSOA supplies the timers for a newly created zone.
//
// The values follow the recommendations in RFC 1912 §2.2: a refresh interval
// short enough that a secondary notices a change within an hour even if NOTIFY
// is lost, a retry well below it, an expiry long enough to survive a weekend
// outage of the primary, and a negative-cache TTL kept short so a mistake can
// be corrected quickly.
func DefaultSOA(primary, mbox Name) SOA {
	return SOA{
		NS:      primary,
		Mbox:    mbox,
		Serial:  NewSerial(1),
		Refresh: 3600,    // 1 hour
		Retry:   900,     // 15 minutes
		Expire:  1209600, // 14 days
		Minimum: 3600,    // 1 hour
		TTL:     3600,
	}
}

// Validate reports whether the SOA parameters are usable.
//
// It enforces the rules that would produce a broken zone, not the style
// recommendations of RFC 1912: a secondary that cannot refresh, or an expiry
// that fires before a retry has had a chance, is a fault rather than a matter
// of taste.
func (s SOA) Validate() error {
	if s.NS.IsZero() {
		return fmt.Errorf("%w: the SOA has no primary name server", ErrInvalid)
	}
	if s.NS.IsRoot() {
		return fmt.Errorf("%w: the SOA primary name server cannot be the root", ErrInvalid)
	}
	if s.Mbox.IsZero() {
		return fmt.Errorf("%w: the SOA has no administrator mailbox", ErrInvalid)
	}

	for _, f := range []struct {
		name  string
		value TTL
	}{
		{"refresh", s.Refresh},
		{"retry", s.Retry},
		{"expire", s.Expire},
		{"minimum", s.Minimum},
		{"TTL", s.TTL},
	} {
		if !f.value.Valid() {
			return fmt.Errorf("%w: SOA %s of %d exceeds the maximum of %d (RFC 2181 §8)",
				ErrInvalid, f.name, f.value, MaxTTL)
		}
	}

	if s.Refresh == 0 {
		return fmt.Errorf("%w: SOA refresh of zero would leave secondaries never refreshing", ErrInvalid)
	}
	if s.Retry == 0 {
		return fmt.Errorf("%w: SOA retry of zero would leave a failed transfer never retried", ErrInvalid)
	}
	// RFC 1912 §2.2: expiry has to outlast at least one refresh and a few
	// retries, or a secondary drops the zone while it is still trying.
	if s.Expire <= s.Refresh+s.Retry {
		return fmt.Errorf(
			"%w: SOA expire of %d must exceed refresh plus retry (%d), or a secondary "+
				"discards the zone before it has finished retrying",
			ErrInvalid, s.Expire, s.Refresh+s.Retry)
	}

	return nil
}

// RData returns the SOA in the presentation form of a record's data, which is
// what a zonefile writes and what the snapshot builder hands to the wire.
func (s SOA) RData() string {
	var b strings.Builder
	b.WriteString(s.NS.String())
	b.WriteByte(' ')
	b.WriteString(s.Mbox.String())
	b.WriteByte(' ')
	b.WriteString(s.Serial.String())
	for _, v := range []TTL{s.Refresh, s.Retry, s.Expire, s.Minimum} {
		b.WriteByte(' ')
		b.WriteString(v.String())
	}
	return b.String()
}

// soaFields is how many whitespace-separated values an SOA's data carries:
// MNAME, RNAME and the five 32-bit counters (RFC 1035 §3.3.13).
const soaFields = 7

// ParseSOAData reads the presentation form of an SOA's record data back into
// the parameters it came from.
func ParseSOAData(s string) (SOA, error) {
	f := strings.Fields(s)
	if len(f) != soaFields {
		return SOA{}, fmt.Errorf(
			"%w: an SOA carries %d values (MNAME, RNAME and five counters), %q has %d",
			ErrInvalid, soaFields, s, len(f))
	}

	ns, err := ParseName(f[0])
	if err != nil {
		return SOA{}, fmt.Errorf("%w: the SOA primary name server: %w", ErrInvalid, err)
	}
	mbox, err := ParseName(f[1])
	if err != nil {
		return SOA{}, fmt.Errorf("%w: the SOA mailbox: %w", ErrInvalid, err)
	}

	serial, err := strconv.ParseUint(f[2], 10, 32)
	if err != nil {
		return SOA{}, fmt.Errorf("%w: the SOA serial %q is not a 32-bit number: %w",
			ErrInvalid, f[2], err)
	}

	out := SOA{NS: ns, Mbox: mbox, Serial: NewSerial(uint32(serial))}
	for i, dst := range []struct {
		what string
		p    *TTL
	}{
		{"refresh interval", &out.Refresh},
		{"retry interval", &out.Retry},
		{"expiry", &out.Expire},
		{"negative-caching TTL", &out.Minimum},
	} {
		v, perr := strconv.ParseUint(f[3+i], 10, 32)
		if perr != nil {
			return SOA{}, fmt.Errorf("%w: the SOA %s %q is not a 32-bit number: %w",
				ErrInvalid, dst.what, f[3+i], perr)
		}
		// The counters share the TTL type and therefore its ceiling, which is
		// lower than 32 bits (RFC 2181 §8).
		if TTL(v) > MaxTTL {
			return SOA{}, fmt.Errorf("%w: the SOA %s of %d exceeds the maximum of %d",
				ErrInvalid, dst.what, v, MaxTTL)
		}
		*dst.p = TTL(v)
	}
	return out, nil
}

// NegativeTTL returns the TTL to put on a negative answer for this zone.
//
// RFC 2308 §3 and §5 make it the lesser of the SOA record's own TTL and the
// SOA MINIMUM field, so that shortening either one takes effect.
func (s SOA) NegativeTTL() TTL {
	if s.TTL < s.Minimum {
		return s.TTL
	}
	return s.Minimum
}
