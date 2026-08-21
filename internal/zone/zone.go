package zone

import (
	"fmt"
	"net/netip"
	"time"
)

// ZoneID identifies a zone. It is a ULID: assignable before the transaction
// that stores it, safe to merge across nodes once the cluster exists, and
// ordered by time so it keeps index locality.
type ZoneID string //nolint:revive // "zone.ZoneID" reads better at call sites than "zone.ID"

// Kind distinguishes a forward zone from a reverse one.
type Kind string

const (
	// KindForward is an ordinary zone mapping names to data.
	KindForward Kind = "forward"
	// KindReverse is a zone under in-addr.arpa or ip6.arpa, mapping addresses
	// back to names.
	KindReverse Kind = "reverse"
)

// Zone is a namespace this server is authoritative for.
type Zone struct {
	ID   ZoneID
	Name Name

	Kind Kind
	// Prefix is the network a reverse zone answers for, derived from its name
	// when the zone is created. It is the zero value for a forward zone.
	Prefix netip.Prefix

	SOA SOA

	// DefaultTTL is applied to a record added without one.
	DefaultTTL TTL
	// AutoReverse enables PTR generation for records in this zone. Nil inherits
	// the global setting.
	AutoReverse *bool

	Disabled bool
	Comment  string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewZone builds a zone for the given apex, deriving its kind and, for a
// reverse zone, the network it is responsible for.
func NewZone(name Name, soa SOA) (Zone, error) {
	if name.IsZero() {
		return Zone{}, fmt.Errorf("%w: a zone needs a name", ErrInvalid)
	}

	z := Zone{
		Name:       name,
		Kind:       KindForward,
		SOA:        soa,
		DefaultTTL: 3600,
	}

	if IsReverseName(name) {
		prefix, err := ParseReversePrefix(name)
		if err != nil {
			return Zone{}, err
		}
		z.Kind = KindReverse
		z.Prefix = prefix
	}

	if err := z.Validate(); err != nil {
		return Zone{}, err
	}
	return z, nil
}

// Validate reports whether the zone itself is well formed. It says nothing
// about the records in it; that is [ValidateRRset] and the applier's job.
func (z Zone) Validate() error {
	if z.Name.IsZero() {
		return fmt.Errorf("%w: a zone needs a name", ErrInvalid)
	}
	if !z.DefaultTTL.Valid() {
		return fmt.Errorf("%w: default TTL of %d exceeds the maximum of %d (RFC 2181 §8)",
			ErrInvalid, z.DefaultTTL, MaxTTL)
	}
	if err := z.SOA.Validate(); err != nil {
		return err
	}

	switch z.Kind {
	case KindForward:
		if z.Prefix.IsValid() {
			return fmt.Errorf("%w: a forward zone cannot carry a network prefix", ErrInvalid)
		}
		if IsReverseName(z.Name) {
			return fmt.Errorf("%w: %q lies in a reverse namespace but is marked as a forward zone",
				ErrInvalid, z.Name)
		}

	case KindReverse:
		if !z.Prefix.IsValid() {
			return fmt.Errorf("%w: a reverse zone needs the network it answers for", ErrInvalid)
		}
		derived, err := ParseReversePrefix(z.Name)
		if err != nil {
			return err
		}
		if derived != z.Prefix {
			return fmt.Errorf("%w: %q names the network %v, but the zone carries %v",
				ErrInvalid, z.Name, derived, z.Prefix)
		}

	default:
		return fmt.Errorf("%w zone kind %q", ErrInvalid, z.Kind)
	}

	return nil
}

// Covers reports whether addr falls inside the network a reverse zone answers
// for. It is always false for a forward zone.
func (z Zone) Covers(addr netip.Addr) bool {
	return z.Kind == KindReverse && z.Prefix.Contains(addr)
}

// Contains reports whether n lies at or below the zone apex. It does not
// account for delegations: a name below an NS record inside this zone is still
// within the zone's namespace, just not answered from it.
func (z Zone) Contains(n Name) bool {
	return !n.IsZero() && n.IsSubDomainOf(z.Name)
}

// IsApex reports whether n is the zone apex, where the SOA and the zone's own
// NS records live.
func (z Zone) IsApex(n Name) bool {
	return z.Name.Equal(n)
}
