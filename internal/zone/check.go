package zone

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// MaxFindings is how many problems a [Check] collects before it stops
// collecting. A zone past this many broken names has something systematically
// wrong with it, and a list that long has stopped being something a person
// reads through.
const MaxFindings = 1000

// Severity says whether the write path would refuse a finding. It is the only
// distinction a check draws, and it is not a scale: anything wanting finer
// grading is asking for a different report (D31).
type Severity string

const (
	// SeverityError is a finding the write path refuses. A zone holding one
	// holds data that is unanswerable or contradictory, and this server would
	// not have let anybody build it, so something reached the database another
	// way.
	SeverityError Severity = "error"

	// SeverityWarning is a finding the write path accepts and would accept
	// again: correct DNS that is probably not what somebody meant, and may be
	// exactly what they meant.
	SeverityWarning Severity = "warning"
)

// FindingScope says which rule a finding came from, so that a client can group
// or filter without reading the sentence.
type FindingScope string

const (
	// ScopeOwner covers the records sharing one name: a CNAME beside other
	// data, a TTL that differs across an RRset, a duplicate, a PTR outside the
	// network its zone answers for.
	ScopeOwner FindingScope = "owner"

	// ScopeDelegation covers where a name sits relative to the zone's
	// delegations: what may live at one, and what may remain below it.
	ScopeDelegation FindingScope = "delegation"

	// ScopeZone covers the zone as a whole, such as the NS records at its apex.
	ScopeZone FindingScope = "zone"

	// ScopeNameServer covers the name servers this zone points at, and whether
	// it answers for the ones that live inside it.
	ScopeNameServer FindingScope = "nameserver"

	// ScopeReverse covers the records reverse automation would generate and
	// this zone does not have.
	ScopeReverse FindingScope = "reverse"
)

// Addressed answers whether a name has an address record in this zone. The
// check asks it about the name servers the zone points at, which is a question
// for whatever holds the records rather than for a walk over them.
type Addressed func(Name) (bool, error)

// Finding is one thing wrong with a zone as it is stored.
//
// It is not an error, because a check reports rather than refuses: an operator
// wants the list, and stopping at the first problem turns a report into a game
// of twenty questions.
type Finding struct {
	// Severity is whether the write path would have refused it.
	Severity Severity

	// Scope is the rule that produced it.
	Scope FindingScope

	// Name is the owner the finding is about, or the apex where the finding is
	// about the zone.
	Name Name

	// Detail is the sentence a person reads. It is the same text the write path
	// refuses with, so a zone repaired until the check is quiet is a zone the
	// write path would accept.
	Detail string
}

// Report is what a check found.
type Report struct {
	// Findings are the problems, in the order the names were read.
	Findings []Finding

	// Truncated is true when [MaxFindings] was reached and the check stopped
	// collecting. What is in the report is still true; it is not everything.
	Truncated bool

	// Records is how many records were read, whether or not they were sound.
	Records int
}

// Sound reports whether the check found nothing at all.
func (r Report) Sound() bool { return len(r.Findings) == 0 }

// Errors counts the findings the write path would refuse. It is the number to
// act on: a warning may be exactly what somebody meant.
func (r Report) Errors() int {
	n := 0
	for _, f := range r.Findings {
		if f.Severity == SeverityError {
			n++
		}
	}
	return n
}

// Check reports everything wrong with a zone, rather than the first thing.
//
// Records are added in canonical order, which is the order the store keeps
// them in, so the whole zone is never held: a check carries the records at one
// name and the delegation points above it. That is what lets it run against a
// zone of the size D12 aims at.
//
// It reuses the rules the write path refuses with rather than restating them,
// which is what D5a asks for: one rule in one function, so that a zone cannot
// pass the check and be refused a moment later. A name with more than one
// problem yields the first of them, because that is the granularity those
// functions answer at.
type Check struct {
	zone Zone
	rep  Report

	// name is the owner being collected, and owned its records.
	name  Name
	owned []Record
	open  bool

	// delegations are the delegation points seen so far that still lie above
	// the name being read, outermost first. Canonical order puts a name before
	// everything beneath it, so a delegation is always known by the time the
	// names it covers arrive.
	delegations []Name

	apexNS bool

	// nsRecords are the NS records seen so far. A zone names a handful of
	// name servers however large it is, so holding them costs nothing and
	// means the rule about them runs in one place.
	nsRecords []Record
}

// NewCheck starts a check of z.
func NewCheck(z Zone) *Check { return &Check{zone: z} }

// Add takes the next record.
//
// Records must arrive in canonical name order, grouped by name, which is what
// the store's own record order already produces. Out of order, the delegation
// rules are checked against whatever was known at the time and the report is
// not to be trusted.
func (c *Check) Add(r *Record) {
	c.rep.Records++

	if c.open && !r.Name.Equal(c.name) {
		c.flush()
	}
	if !c.open {
		c.name, c.owned, c.open = r.Name, c.owned[:0], true
	}
	c.owned = append(c.owned, *r)

	if r.Type == TypeNS {
		if c.zone.IsApex(r.Name) {
			c.apexNS = true
		}
		c.nsRecords = append(c.nsRecords, *r)
	}
}

// Done finishes the last name and returns what the check found.
//
// addressed is asked, once per name server this zone points at that lives
// inside it, whether the zone answers for it with an address. That cannot be
// settled while walking, because a name server is named anywhere in the zone
// and its address record may already have gone past.
func (c *Check) Done(addressed Addressed) (Report, error) {
	if c.open {
		c.flush()
	}

	// RFC 1034 §4.2.1, checked here rather than per name because it is the
	// absence of a record that is wrong and no name carries an absence.
	if !c.apexNS {
		c.record(Finding{
			Severity: SeverityError,
			Scope:    ScopeZone,
			Name:     c.zone.Name,
			Detail: fmt.Sprintf(
				"the zone %q has no NS record at its apex; a zone must name at least one "+
					"authoritative server (RFC 1034 §4.2.1)", c.zone.Name),
		})
	}

	if err := c.lame(addressed); err != nil {
		return Report{}, err
	}
	return c.rep, nil
}

// lame reports the name servers this zone points at and has no address for. A
// resolver referred to one is told, authoritatively, that the name does not
// exist, and the delegation is lame (RFC 1912 §2.8).
//
// A warning rather than an error: the write path accepts this and would accept
// it again, because the address record may simply not have been written yet
// (D31).
func (c *Check) lame(addressed Addressed) error {
	for _, ns := range NameServersInside(c.zone, c.nsRecords) {
		ok, err := addressed(ns.Target)
		if err != nil {
			return err
		}
		if ok {
			continue
		}
		c.record(Finding{
			Severity: SeverityWarning,
			Scope:    ScopeNameServer,
			Name:     ns.Owner,
			Detail:   LameDetail(ns.Target),
		})
	}
	return nil
}

// NameServer is one name server a zone points at, with the owner of the NS
// record that names it: the apex for the zone's own servers, a child name for
// a delegation.
type NameServer struct {
	Owner  Name
	Target Name
}

// NameServersInside returns the name servers these NS records name that live
// inside the zone, in canonical order and each once. One outside the zone is
// somebody else's to answer for, and two records naming one server are one
// question.
//
// It is separate from the walk so that the zone's own read can ask the same
// question from the NS records alone, without reading every record in the zone
// to find out (D31).
func NameServersInside(z Zone, ns []Record) []NameServer {
	seen := make(map[Name]NameServer, len(ns))
	for i := range ns {
		r := &ns[i]
		if r.Type != TypeNS {
			continue
		}
		target, err := ParseName(r.RData.String())
		if err != nil || !z.Contains(target) {
			continue
		}
		if _, dup := seen[target]; !dup {
			seen[target] = NameServer{Owner: r.Name, Target: target}
		}
	}

	out := make([]NameServer, 0, len(seen))
	for _, s := range seen {
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b NameServer) int { return a.Target.Compare(b.Target) })
	return out
}

// LameDetail is the sentence about a name server a zone points at and has no
// address for.
func LameDetail(target Name) string {
	return fmt.Sprintf(
		"%s has no address in this zone, so a resolver referred to it is told the name does "+
			"not exist. Add %s A <address>, or point the delegation somewhere off-site "+
			"(RFC 1912 §2.8).", target, target)
}

// flush checks the name that has just finished.
func (c *Check) flush() {
	defer func() { c.open = false }()

	c.popPast(c.name)
	if !c.zone.IsApex(c.name) && hasType(c.owned, TypeNS) {
		c.delegations = append(c.delegations, c.name)
	}

	if err := ValidateOwner(c.zone, c.name, c.owned); err != nil {
		c.record(finding(ScopeOwner, c.name, err))
	}

	var point Name
	if n := len(c.delegations); n > 0 {
		point = c.delegations[n-1]
	}
	if err := ValidateUnderDelegation(c.name, c.owned, point); err != nil {
		c.record(finding(ScopeDelegation, c.name, err))
	}
}

// popPast drops the delegation points that name does not lie under.
func (c *Check) popPast(name Name) {
	for len(c.delegations) > 0 {
		if name.IsSubDomainOf(c.delegations[len(c.delegations)-1]) {
			return
		}
		c.delegations = c.delegations[:len(c.delegations)-1]
	}
}

// record adds a finding, up to the point where the list stops being readable.
func (c *Check) record(f Finding) {
	if len(c.rep.Findings) >= MaxFindings {
		c.rep.Truncated = true
		return
	}
	c.rep.Findings = append(c.rep.Findings, f)
}

// finding turns what a validator refused with into something a client can
// render. The sentence is kept exactly as the write path words it; only the
// "invalid" prefix that made it an error goes.
func finding(scope FindingScope, name Name, err error) Finding {
	detail := err.Error()
	if errors.Is(err, ErrInvalid) {
		detail = strings.TrimPrefix(detail, ErrInvalid.Error()+": ")
	}
	return Finding{Severity: SeverityError, Scope: scope, Name: name, Detail: detail}
}

// MissingReverseDetail is the sentence about an entry reverse automation would
// generate for a record and has not.
func MissingReverseDetail(r Record) string {
	return fmt.Sprintf(
		"a %s at %s naming %s is missing, so the reverse of that address answers nothing. "+
			"Reverse automation writes it when the record it follows from is next saved, or "+
			"when this zone is reconciled.", r.Type, r.Name, r.RData)
}
