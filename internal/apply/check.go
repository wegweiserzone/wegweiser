package apply

import (
	"context"

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

	b, _, err := a.PlanReconcile(ctx, zid, Meta{Source: journal.SourceSystem})
	if err != nil {
		return nil, err
	}
	if b.Empty() {
		return nil, nil
	}

	var out []zone.Finding
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
