// Package zonefile reads and writes zone data in the presentation format of
// RFC 1035 §5: what everybody calls a zonefile.
//
// It is an import and export format and never a storage format (architecture
// invariant 5). Nothing here is on the query path and nothing here is what the
// server answers from: a file is read into records, the records go through the
// ordinary write path, and what the server serves is the database. Exporting
// runs the same trip backwards, so that moving off this server takes as long
// as moving onto it.
//
// The lexing is the wire library's, for the reason ADR 0005 gives about RDATA:
// RFC 1035 §5 is line continuations, parentheses, comments, `@`, relative
// names, omitted owners, omitted classes, omitted TTLs and a presentation
// syntax per record type, and re-deriving it would produce something slightly
// worse than a parser that has been carrying production zones for a decade.
// What this package adds is the policy, which files we accept, what a zone
// has to contain to be one, and what a file becomes once it is read.
package zonefile

import (
	"fmt"
	"io"
	"strings"

	wire "github.com/miekg/dns"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// DefaultMaxRecords bounds what one file may turn into.
//
// The bound is not about file size. `$GENERATE 1-16777215 $ PTR host-$` is
// thirty octets and expands to sixteen million records, so a limit on what
// arrives says nothing about what it becomes. A million is the zone size
// docs/decisions.md D12 designs for, so a file that produces more than that is
// past what this server is built to hold either way.
const DefaultMaxRecords = 1_000_000

// Content is what a zonefile holds, once read.
//
// The SOA is separate from the records because it is separate in the model: it
// is the zone's own settings rather than a record somebody edits, and its
// serial belongs to the journal (data model §4.1).
type Content struct {
	// Origin is the zone apex, taken from the owner name of the SOA. The file
	// says which zone it describes; the caller does not have to know.
	Origin zone.Name

	// SOA is the start of authority as the file gives it, serial included. An
	// import seeds from that serial rather than resetting it: see
	// docs/decisions.md D2.
	SOA zone.SOA

	// Records is everything else, with no zone identifier yet: the file does
	// not know which zone row it will land in, and inventing one here would be
	// a second place that mints identifiers.
	Records []zone.Record
}

// Options configure a read.
type Options struct {
	// Origin is where relative names are resolved from, for a file that does
	// not set $ORIGIN itself. The root is used when it is not given, which is
	// almost certainly not what a file of relative names meant, so a file
	// like that is refused for having its records outside its own apex rather
	// than being imported into the wrong names.
	Origin zone.Name

	// DefaultTTL applies to a record that carries no TTL in a file that sets
	// no $TTL. Zero leaves it to the file, and a file that then omits one is
	// refused by the parser rather than given a number nobody chose.
	DefaultTTL zone.TTL

	// MaxRecords bounds how many records the file may produce. Zero takes
	// [DefaultMaxRecords]; a negative value removes the bound, which is for a
	// caller that has already bounded it some other way.
	MaxRecords int
}

// Parse reads a zonefile.
//
// The zone it describes is whichever name its SOA sits at: a file carries that
// with it, so a caller does not have to be told twice and cannot be told
// inconsistently. Exactly one SOA is required: a file without one is a
// fragment rather than a zone, and a file with two does not say which.
func Parse(r io.Reader, opts Options) (*Content, error) {
	origin := "."
	if !opts.Origin.IsZero() {
		origin = opts.Origin.String()
	}

	// The file name is only ever used in the library's error messages and to
	// resolve $INCLUDE. $INCLUDE stays disabled; it is off by default, and it
	// has to be: a file arriving over the API that could pull in a path would
	// read this server's filesystem out loud.
	zp := wire.NewZoneParser(r, origin, "")
	if opts.DefaultTTL != 0 {
		zp.SetDefaultTTL(uint32(opts.DefaultTTL))
	}

	limit := opts.MaxRecords
	if limit == 0 {
		limit = DefaultMaxRecords
	}

	var (
		out     Content
		haveSOA bool
	)
	for rr, ok := zp.Next(); ok; rr, ok = zp.Next() {
		if rr.Header().Rrtype == wire.TypeSOA {
			if haveSOA {
				return nil, fmt.Errorf(
					"%w: the file has more than one SOA record, so it does not say which zone it describes",
					zone.ErrInvalid)
			}
			soa, name, err := soaFrom(rr)
			if err != nil {
				return nil, err
			}
			out.Origin, out.SOA, haveSOA = name, soa, true
			continue
		}

		if limit > 0 && len(out.Records) >= limit {
			return nil, fmt.Errorf(
				"%w: the file produces more than %d records; a directive such as $GENERATE can "+
					"expand a single line into millions, so the bound is on what a file becomes "+
					"rather than on how long it is", zone.ErrInvalid, limit)
		}

		rec, err := recordFrom(rr)
		if err != nil {
			return nil, err
		}
		out.Records = append(out.Records, rec)
	}
	if err := zp.Err(); err != nil {
		return nil, fmt.Errorf("%w: %s", zone.ErrInvalid, cleanError(err))
	}

	if !haveSOA {
		return nil, fmt.Errorf(
			"%w: the file has no SOA record, so it is a fragment rather than a zone",
			zone.ErrInvalid)
	}
	for i := range out.Records {
		if !out.Records[i].Name.IsSubDomainOf(out.Origin) {
			return nil, fmt.Errorf(
				"%w: the %s record at %q lies outside %q, the zone this file's SOA describes",
				zone.ErrInvalid, out.Records[i].Type, out.Records[i].Name, out.Origin)
		}
	}
	return &out, nil
}

// soaFrom reads the zone's own settings out of an SOA record.
func soaFrom(rr wire.RR) (zone.SOA, zone.Name, error) {
	name, err := zone.ParseName(rr.Header().Name)
	if err != nil {
		return zone.SOA{}, zone.Name{}, fmt.Errorf(
			"%w: the SOA sits at %q, which is not a name: %w", zone.ErrInvalid, rr.Header().Name, err)
	}

	data, err := zone.RDataFromRR(rr)
	if err != nil {
		return zone.SOA{}, zone.Name{}, fmt.Errorf("%w: the SOA of %q: %w", zone.ErrInvalid, name, err)
	}
	soa, err := zone.ParseSOAData(data.String())
	if err != nil {
		return zone.SOA{}, zone.Name{}, fmt.Errorf("the SOA of %q: %w", name, err)
	}
	// The TTL is the record's own and is not part of the data.
	ttl := zone.TTL(rr.Header().Ttl)
	if !ttl.Valid() {
		return zone.SOA{}, zone.Name{}, fmt.Errorf(
			"%w: the SOA of %q has a TTL of %d, over the maximum of %d (RFC 2181 §8)",
			zone.ErrInvalid, name, rr.Header().Ttl, zone.MaxTTL)
	}
	soa.TTL = ttl

	if err := soa.Validate(); err != nil {
		return zone.SOA{}, zone.Name{}, fmt.Errorf("the SOA of %q: %w", name, err)
	}
	return soa, name, nil
}

// recordFrom turns one parsed record into the model's.
func recordFrom(rr wire.RR) (zone.Record, error) {
	h := rr.Header()

	name, err := zone.ParseName(h.Name)
	if err != nil {
		return zone.Record{}, fmt.Errorf("%w: %q is not a name: %w", zone.ErrInvalid, h.Name, err)
	}
	data, err := zone.RDataFromRR(rr)
	if err != nil {
		return zone.Record{}, fmt.Errorf("%w: the record at %q: %w", zone.ErrInvalid, name, err)
	}

	rec := zone.Record{
		Name:  name,
		Class: zone.Class(h.Class),
		Type:  zone.RRType(h.Rrtype),
		TTL:   zone.TTL(h.Ttl),
		RData: data,
	}
	if err := rec.Validate(); err != nil {
		return zone.Record{}, fmt.Errorf("the record at %q: %w", name, err)
	}
	return rec, nil
}

// cleanError makes the library's parse errors readable.
func cleanError(err error) string {
	msg := strings.TrimPrefix(err.Error(), "dns: ")
	if at := strings.LastIndex(msg, " at line: "); at >= 0 {
		return msg[at+len(" at line: "):] + ": " + msg[:at]
	}
	return msg
}
