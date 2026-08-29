package apply

import (
	"context"
	"fmt"

	"github.com/wegweiserzone/wegweiser/internal/journal"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// CheckReverse reports the reverse entries this zone's records imply and does
// not have.
//
// It plans exactly what [Applier.Reconcile] would carry out and reports it
// instead of writing it, so the rules deciding what an address record
// generates run in one place rather than two that drift (D21).
//
// The inverse question, an entry that is here and would not be generated
// today, is deliberately not asked. Reverse automation only ever adds: an
// entry made obsolete is taken away by the change that obsoleted it, and one
// somebody detached is theirs to keep (D4). Reporting those would need a rule
// for what may legitimately remain, which D4 leaves to the person.
func (a *Applier) CheckReverse(
	ctx context.Context, zid zone.ZoneID,
) ([]zone.Finding, error) {
	// The zone lock, because planning reads the zone as it stands and a write
	// landing underneath would make the answer describe neither state.
	unlock := a.locks.lock(string(zid))
	defer unlock()

	b, res, err := a.PlanReconcile(ctx, zid, Meta{Source: journal.SourceSystem})
	if err != nil {
		return nil, err
	}

	// The refusals first, because they are the answer to a question the caller
	// did not know to ask: an entry that is missing because something else
	// holds the name is not the same as one nobody has written yet (D33).
	var out []zone.Finding
	// Indexed rather than ranged by value: a Conflict is a large struct and
	// this reads a handful of its fields.
	for i := range res.Conflicts {
		c := &res.Conflicts[i]
		out = append(out, zone.Finding{
			Severity: zone.SeverityWarning,
			Scope:    zone.ScopeReverse,
			Name:     c.ReverseName,
			Detail:   conflictDetail(c),
		})
	}

	if b.Empty() {
		return out, nil
	}
	for _, zoneID := range b.set.order {
		ch := b.set.byZone[zoneID]
		if ch == nil {
			continue
		}
		for i := range ch.inserts {
			rec := &ch.inserts[i]
			out = append(out, zone.Finding{
				Severity: zone.SeverityWarning,
				Scope:    zone.ScopeReverse,
				Name:     rec.Name,
				Detail:   zone.MissingReverseDetail(*rec),
			})
		}
	}
	return out, nil
}

// conflictDetail is the sentence about an address more than one name claims.
//
// Worded as the state it is rather than as a failure, because several names on
// one address is the ordinary case (D3): a virtual host, a load balancer, a
// service alias. What is worth saying is which of them the reverse answers
// with, since only one of them can.
func conflictDetail(c *Conflict) string {
	held := ", which somebody wrote by hand"
	if c.Generated {
		held = ""
	}
	return fmt.Sprintf(
		"%s is answered in reverse as %s%s, and %s points at it as well. A reverse lookup "+
			"names one host, so %s has no entry of its own here.",
		c.Address, c.Existing, held, c.SourceName, c.SourceName)
}
