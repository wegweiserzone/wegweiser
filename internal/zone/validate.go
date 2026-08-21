package zone

import (
	"fmt"
)

// ValidateRRset reports whether a set of records forming one RRset is well
// formed.
//
// An RRset is everything sharing an owner name, class and type, and DNS answers
// it as a unit (RFC 2181 §5). Two rules follow from that and cannot be checked
// on a single record:
//
//   - every member carries the same TTL (RFC 2181 §5.2), because a resolver
//     caches the set as one thing and a divergent TTL makes the answer depend
//     on which copy it saw;
//   - no member repeats another's data (RFC 2181 §5).
func ValidateRRset(records []Record) error {
	if len(records) == 0 {
		return nil
	}

	key := records[0].Key()
	ttl := records[0].TTL
	seen := make(map[string]struct{}, len(records))

	// Indexed rather than ranged by value: a Record is large, and this walks
	// every record of a zone during an import.
	for i := range records {
		r := &records[i]
		if err := r.Validate(); err != nil {
			return err
		}
		if r.Key() != key {
			return fmt.Errorf("%w: %s is not part of the %s %s RRset at %q",
				ErrInvalid, r.Type, key.Class, key.Type, key.Name)
		}
		if r.TTL != ttl {
			return fmt.Errorf(
				"%w: the %s records at %q have different TTLs (%d and %d); every record in "+
					"an RRset must share one TTL (RFC 2181 §5.2)",
				ErrInvalid, key.Type, key.Name, ttl, r.TTL)
		}
		if _, dup := seen[r.RData.String()]; dup {
			return fmt.Errorf("%w: %q appears twice in the %s RRset at %q (RFC 2181 §5)",
				ErrInvalid, r.RData, key.Type, key.Name)
		}
		seen[r.RData.String()] = struct{}{}
	}

	// RFC 1034 §3.6.2 and RFC 2181 §10.1: a name has at most one canonical
	// name, so a second CNAME is not an alternative but a contradiction.
	if key.Type == TypeCNAME && len(records) > 1 {
		return fmt.Errorf(
			"%w: %q has %d CNAME records, but a name can have only one canonical name "+
				"(RFC 2181 §10.1)", ErrInvalid, key.Name, len(records))
	}

	return nil
}

// ValidateOwner reports whether the records sharing one owner name can coexist.
func ValidateOwner(z Zone, name Name, records []Record) error {
	if !z.Contains(name) {
		return fmt.Errorf("%w: %q is not inside the zone %q", ErrInvalid, name, z.Name)
	}

	byType := make(map[RRsetKey][]Record, len(records))
	hasCNAME := false
	otherTypes := 0

	for i := range records {
		r := &records[i]
		if !r.Name.Equal(name) {
			return fmt.Errorf("%w: %q was checked among the records at %q", ErrInvalid, r.Name, name)
		}
		byType[r.Key()] = append(byType[r.Key()], *r)

		if r.Type == TypeCNAME {
			hasCNAME = true
		} else {
			otherTypes++
		}
	}

	for _, set := range byType {
		if err := ValidateRRset(set); err != nil {
			return err
		}
	}

	// RFC 2181 §10.1: where a CNAME is present, no other data may be. A
	// resolver following the alias would otherwise have two answers and no way
	// to choose.
	if hasCNAME && otherTypes > 0 {
		return fmt.Errorf(
			"%w: %q has a CNAME alongside other records; a CNAME means \"look up this other "+
				"name instead\", so nothing else can live at that name (RFC 2181 §10.1)",
			ErrInvalid, name)
	}
	// The apex holds the SOA and the zone's own NS records, so it can never be
	// an alias for somewhere else.
	if hasCNAME && z.IsApex(name) {
		return fmt.Errorf(
			"%w: %q is the zone apex and cannot be a CNAME; the SOA and the zone's NS "+
				"records live there", ErrInvalid, name)
	}

	// A PTR outside the network its zone answers for would never be found,
	// since a resolver derives the name it asks for from the address.
	if z.Kind == KindReverse {
		for i := range records {
			r := &records[i]
			if r.Type != TypePTR {
				continue
			}
			if prefix, err := ParseReversePrefix(r.Name); err == nil && !z.Prefix.Contains(prefix.Addr()) {
				return fmt.Errorf("%w: the PTR at %q names an address outside %v, the network %q answers for",
					ErrInvalid, r.Name, z.Prefix, z.Name)
			}
		}
	}

	return nil
}

// ValidateZone reports whether a complete set of records forms a usable zone.
func ValidateZone(z Zone, records []Record) error {
	if err := z.Validate(); err != nil {
		return err
	}

	byOwner := make(map[Name][]Record)
	delegations := make(map[Name]struct{})

	for i := range records {
		r := &records[i]
		if !z.Contains(r.Name) {
			return fmt.Errorf("%w: %q is not inside the zone %q", ErrInvalid, r.Name, z.Name)
		}
		byOwner[r.Name] = append(byOwner[r.Name], *r)
		if r.Type == TypeNS && !z.IsApex(r.Name) {
			delegations[r.Name] = struct{}{}
		}
	}

	for name, owned := range byOwner {
		if err := ValidateOwner(z, name, owned); err != nil {
			return err
		}
	}

	// RFC 1034 §4.2.1: a zone is delimited by NS records at its apex. Without
	// them the zone names no authority for itself and no parent can delegate
	// to it.
	if !hasType(byOwner[z.Name], TypeNS) {
		return fmt.Errorf(
			"%w: the zone %q has no NS record at its apex; a zone must name at least one "+
				"authoritative server (RFC 1034 §4.2.1)", ErrInvalid, z.Name)
	}

	if err := validateDelegations(byOwner, delegations); err != nil {
		return err
	}

	return nil
}

// validateDelegations checks what may live at and below every delegation point
// a complete zone holds.
func validateDelegations(byOwner map[Name][]Record, delegations map[Name]struct{}) error {
	for name, owned := range byOwner {
		point, ok := closestDelegation(name, delegations)
		if !ok {
			point = Name{}
		}
		if err := ValidateUnderDelegation(name, owned, point); err != nil {
			return err
		}
	}
	return nil
}

// ValidateUnderDelegation reports whether the records at one name may live
// where the zone's delegations put it.
//
// delegation is the closest delegation point at or above name; a zero name
// means there is none and nothing to check. It is a parameter rather than
// something worked out here, because finding it means either a whole zone in
// memory or a walk up the database, and the two callers have one each.
func ValidateUnderDelegation(name Name, records []Record, delegation Name) error {
	if delegation.IsZero() {
		return nil
	}

	if delegation.Equal(name) {
		// RFC 1034 §4.2.1: at a delegation the parent zone keeps only the NS
		// records. Anything else there is invisible, because a query for that
		// name is referred to the child.
		for i := range records {
			if r := &records[i]; r.Type != TypeNS {
				return fmt.Errorf(
					"%w: %q delegates to another zone, so its %s record would never be "+
						"answered; a query for that name is referred to the child (RFC 1034 §4.2.1)",
					ErrInvalid, name, r.Type)
			}
		}
		return nil
	}

	// Below a delegation only glue may remain: address records that let a
	// resolver reach the child's servers, which it could not otherwise look up
	// without asking the zone it is trying to find.
	for i := range records {
		if r := &records[i]; r.Type != TypeA && r.Type != TypeAAAA {
			return fmt.Errorf(
				"%w: %q lies below the delegation at %q, where only A and AAAA glue "+
					"records may remain; the %s record would never be answered",
				ErrInvalid, name, delegation, r.Type)
		}
	}
	return nil
}

// closestDelegation returns the nearest delegation point at or above name.
func closestDelegation(name Name, delegations map[Name]struct{}) (Name, bool) {
	for n := name; ; {
		if _, ok := delegations[n]; ok {
			return n, true
		}
		parent, ok := n.Parent()
		if !ok {
			return Name{}, false
		}
		n = parent
	}
}

// hasType reports whether any record in the slice carries the given type.
func hasType(records []Record, t RRType) bool {
	for i := range records {
		if records[i].Type == t {
			return true
		}
	}
	return false
}
