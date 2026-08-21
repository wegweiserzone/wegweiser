package apply

import (
	"context"
	"errors"

	"github.com/wegweiserzone/wegweiser/internal/journal"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// Reconcile writes the reverse entries a zone's records imply but does not yet
// have.
//
// It exists because reverse automation reacts to changes, and a zone that
// arrives after the records is a case with no change to react to. Creating a
// reverse zone for a network whose addresses are already in use is exactly
// that: the entries would appear one at a time as each address record happened
// to be edited, which is no answer at all. So the zone is created (never as a
// side effect, always because somebody asked (D6)) and then filled.
//
// It only adds. A generated entry that should no longer exist is already taken
// away by the change that made it obsolete, and one that somebody detached is
// theirs to keep (D4): removing it here would make detaching mean nothing.
func (a *Applier) Reconcile(ctx context.Context, zid zone.ZoneID, meta Meta) (*Result, error) {
	if err := meta.Validate(); err != nil {
		return nil, err
	}

	unlock := a.locks.lock(string(zid))
	defer unlock()

	res := &Result{}
	err := a.store.Update(ctx, func(tx store.Tx) error {
		res.Commits, res.Conflicts, res.MissingZones = nil, nil, nil

		z, err := tx.ZoneByID(ctx, zid)
		if err != nil {
			return err
		}

		var cs changeSet
		if err := a.reconcileInto(ctx, tx, &cs, z, res); err != nil {
			return err
		}
		if cs.empty() {
			return nil
		}
		return a.commit(ctx, tx, &cs, z, journal.KindEdit, meta, res)
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// reconcileInto queues everything missing for one zone.
func (a *Applier) reconcileInto(
	ctx context.Context, tx store.Tx, cs *changeSet, z *zone.Zone, res *Result,
) error {
	f := store.RecordFilter{
		Types:  []zone.RRType{zone.TypeA, zone.TypeAAAA},
		Paging: store.Paging{Limit: store.MaxLimit},
	}
	if z.Kind == zone.KindReverse {
		f.Prefix = z.Prefix
	} else {
		if !a.autoReverse(z) {
			return nil
		}
		f.ZoneID = z.ID
	}

	// Read a page at a time rather than streamed. A page is a query that has
	// finished, and the work done for each record asks the same transaction
	// further questions, which it cannot do while a result set is still open
	// on the one connection a transaction has.
	for {
		page, err := tx.ListRecords(ctx, f)
		if err != nil {
			return err
		}
		for _, rec := range page.Items {
			ok, err := a.sourceGenerates(ctx, tx, cs, rec)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			if err := a.supersede(ctx, tx, cs, rec); err != nil {
				return err
			}
			if err := a.generate(ctx, tx, cs, rec, res); err != nil {
				return err
			}
		}
		if page.NextCursor == "" {
			return nil
		}
		f.Cursor = page.NextCursor
	}
}

// supersede retires the entries a record generated that are no longer where
// they belong.
//
// A classless child added under a /24 is the case this exists for. The address
// records were already generating entries into the /24, and those entries are
// not wrong so much as overtaken: the child is the more specific zone, so it is
// where the entry goes now, and the /24 carries the delegation pointing at it
// instead. Left in place the old entry would sit exactly where the delegation
// has to go, and RFC 2181 §10.1 leaves no room for both.
func (a *Applier) supersede(
	ctx context.Context, tx store.Tx, cs *changeSet, rec *zone.Record,
) error {
	addr, ok := rec.Address()
	if !ok {
		return nil
	}
	rev, err := tx.ReverseZoneFor(ctx, addr)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	owner, err := rev.ReverseOwner(addr)
	if err != nil {
		return err
	}

	derived, err := tx.ManagedBy(ctx, rec.ID)
	if err != nil {
		return err
	}
	for _, d := range derived {
		if d.ZoneID == rev.ID && d.Type == zone.TypePTR && d.Name.Equal(owner) {
			continue // already in the right place
		}
		if err := a.retire(ctx, tx, cs, d); err != nil {
			return err
		}
	}
	return nil
}

// sourceGenerates reports whether a record's own zone has the automation on.
// Reconciling a reverse zone reaches records in zones that may each have their
// own answer, and a zone that has turned it off does not get entries written
// for it from the other side.
func (a *Applier) sourceGenerates(
	ctx context.Context, tx store.Tx, cs *changeSet, rec *zone.Record,
) (bool, error) {
	src, err := a.zoneOf(ctx, tx, cs, rec.ZoneID)
	if err != nil {
		return false, err
	}
	return a.autoReverse(src), nil
}
