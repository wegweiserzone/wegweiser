package zone

import (
	"fmt"
	"net/netip"
	"strings"
	"time"
)

// RecordID identifies a record. Like [ZoneID] it is a ULID, so a record has a
// stable identity a diff line can anchor to and the API can address.
type RecordID string

// ManagedKind says why a record was generated rather than authored.
type ManagedKind string

const (
	// ManagedPTR is a PTR derived from an A or AAAA record.
	ManagedPTR ManagedKind = "ptr"
	// ManagedRFC2317CNAME is a CNAME in a parent /24 delegating a single
	// address into a classless child zone (RFC 2317 §4).
	ManagedRFC2317CNAME ManagedKind = "rfc2317-cname"
)

// Record is a single resource record.
//
// Records are stored individually rather than as RRsets, so that a comment,
// provenance and a stable identity can hang off each one. The RRset rules of
// RFC 2181 are enforced by [ValidateRRset] instead of by the shape of the type.
// See docs/decisions/d20-individual-records.md.
type Record struct {
	ID     RecordID
	ZoneID ZoneID

	Name  Name
	Class Class
	Type  RRType
	TTL   TTL
	RData RData

	// ManagedBy links a generated record back to the record that caused it,
	// and is empty for an authored one.
	ManagedBy   RecordID
	ManagedKind ManagedKind

	Comment  string
	Disabled bool

	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewRecord builds a record from presentation-format input, canonicalising the
// data as it goes. It is the constructor the API, the CLI and the zonefile
// importer all funnel through, so nothing enters the model unvalidated.
func NewRecord(zoneID ZoneID, name Name, class Class, typ RRType, ttl TTL, rdata string) (Record, error) {
	r := Record{
		ZoneID: zoneID,
		Name:   name,
		Class:  class,
		Type:   typ,
		TTL:    ttl,
	}

	// The header is checked before the data, so that an RRSIG is turned away
	// with "we do not sign zones yet" rather than with whatever the data parser
	// happens to complain about first.
	if err := r.validateHeader(); err != nil {
		return Record{}, err
	}

	data, err := ParseRData(typ, class, rdata)
	if err != nil {
		return Record{}, err
	}
	r.RData = data

	if err := r.Validate(); err != nil {
		return Record{}, err
	}
	return r, nil
}

// Validate reports whether the record is well formed on its own.
//
// Rules that need the rest of the zone (the CNAME restrictions of RFC 2181
// §10.1, uniform TTLs within an RRset, what a delegation permits below it)
// belong to [ValidateRRset] and [ValidateZoneRecords], because a single record
// cannot see them.
func (r Record) Validate() error {
	if err := r.validateHeader(); err != nil {
		return err
	}
	if r.RData.IsZero() {
		return fmt.Errorf("%w: a %s record needs data", ErrInvalid, r.Type)
	}
	if (r.ManagedBy == "") != (r.ManagedKind == "") {
		return fmt.Errorf("%w: a generated record needs both a source and a reason", ErrInvalid)
	}
	return nil
}

// validateHeader checks everything about a record that does not depend on its
// data, so a record can be turned away for what it is before anyone tries to
// parse what it holds.
func (r Record) validateHeader() error {
	if r.Name.IsZero() {
		return fmt.Errorf("%w: a record needs an owner name", ErrInvalid)
	}
	if !r.Class.Storable() {
		return fmt.Errorf("%w: class %s exists only inside a message, not in a zone (RFC 6895 §3.2)",
			ErrInvalid, r.Class)
	}
	if !r.Type.Storable() {
		return fmt.Errorf("%w: %s exists only inside a message, not in a zone (RFC 6895 §3.1)",
			ErrInvalid, r.Type)
	}
	if r.Type.IsDNSSEC() {
		return fmt.Errorf(
			"%w: %s records are maintained by the signer, and Wegweiser does not sign zones yet, "+
				"so writing one by hand would leave a signature nothing keeps current",
			ErrInvalid, r.Type)
	}
	// The SOA is zone metadata, not a record: its serial belongs to the
	// journal, and one commit advances it by exactly one. As an editable
	// record it would be one careless edit away from breaking every secondary.
	if r.Type == TypeSOA {
		return fmt.Errorf(
			"%w: the SOA is part of the zone rather than a record; edit the zone's "+
				"start-of-authority settings instead", ErrInvalid)
	}
	if !r.TTL.Valid() {
		return fmt.Errorf("%w: TTL of %d exceeds the maximum of %d (RFC 2181 §8)",
			ErrInvalid, r.TTL, MaxTTL)
	}
	return nil
}

// IsManaged reports whether the record was generated from another one rather
// than authored. A generated record is not edited directly; its source is.
func (r Record) IsManaged() bool { return r.ManagedBy != "" }

// Address returns the IP address of an A or AAAA record, and reports false for
// every other type.
func (r Record) Address() (netip.Addr, bool) { return r.RData.Address(r.Type) }

// RRsetKey identifies the set a record belongs to: everything sharing an owner
// name, class and type is one RRset and is answered as a unit (RFC 2181 §5).
type RRsetKey struct {
	Name  Name
	Class Class
	Type  RRType
}

// Key returns the RRset this record belongs to.
func (r Record) Key() RRsetKey {
	return RRsetKey{Name: r.Name, Class: r.Class, Type: r.Type}
}

// String returns the record as one zonefile line.
func (r Record) String() string {
	var b strings.Builder
	b.WriteString(r.Name.String())
	b.WriteByte('\t')
	b.WriteString(r.TTL.String())
	b.WriteByte('\t')
	b.WriteString(r.Class.String())
	b.WriteByte('\t')
	b.WriteString(r.Type.String())
	b.WriteByte('\t')
	b.WriteString(r.RData.String())
	return b.String()
}

// Compare orders records the way a zone is exported and the way the GUI walks
// the name tree: by owner name in the canonical order of RFC 4034 §6.1, then by
// type, then by data.
func (r Record) Compare(o Record) int {
	if c := r.Name.Compare(o.Name); c != 0 {
		return c
	}
	if r.Class != o.Class {
		if r.Class < o.Class {
			return -1
		}
		return 1
	}
	if r.Type != o.Type {
		if r.Type < o.Type {
			return -1
		}
		return 1
	}
	return strings.Compare(r.RData.String(), o.RData.String())
}
