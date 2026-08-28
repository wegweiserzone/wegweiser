package apply

import (
	"context"
	"fmt"

	"github.com/wegweiserzone/wegweiser/internal/id"
	"github.com/wegweiserzone/wegweiser/internal/journal"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// Rollback restores a zone to the state it had at a serial.
//
// It moves forward to that state rather than rewinding to it: the difference
// between the zone as it now stands and the zone as it stood then is written as
// a new commit, marked as a rollback and naming the serial it restores. History
// stays append-only, which is not tidiness but correctness: a secondary that
// has already seen serial 90 will never accept a jump back to 42, because RFC
// 1982 arithmetic makes 42 older and RFC 1995 has no way to express going
// backwards. See data model §3.7.
//
// Records the server generated are not restored. They are derived from other
// records, and the automation puts them back from whatever those now say; a
// rollback that also wrote the derived copies would be racing the thing that
// owns them.
func (a *Applier) Rollback(
	ctx context.Context, zid zone.ZoneID, target zone.Serial, meta Meta,
) (*Result, error) {
	if err := meta.Validate(); err != nil {
		return nil, err
	}

	unlock := a.locks.lock(string(zid))
	defer unlock()

	b, res, err := a.PlanRollback(ctx, zid, target, meta)
	if err != nil {
		return nil, err
	}
	if err := a.ApplyBatch(ctx, b); err != nil {
		return nil, err
	}
	return res, nil
}

// PlanRollback works out what restoring an earlier state would amount to, and
// leaves the store as it found it. See [Applier.Plan].
func (a *Applier) PlanRollback(
	ctx context.Context, zid zone.ZoneID, target zone.Serial, meta Meta,
) (*Batch, *Result, error) {
	if err := meta.Validate(); err != nil {
		return nil, nil, err
	}

	res := &Result{}
	b, err := a.planning(ctx, func(tx store.Tx) (*Batch, error) {
		return a.rollbackIn(ctx, tx, zid, target, meta, res)
	})
	if err != nil {
		return nil, nil, err
	}
	return b, res, nil
}

// rollbackIn works the rollback out inside one transaction.
func (a *Applier) rollbackIn(
	ctx context.Context, tx store.Tx, zid zone.ZoneID, target zone.Serial, meta Meta, res *Result,
) (*Batch, error) {
	z, err := tx.ZoneByID(ctx, zid)
	if err != nil {
		return nil, err
	}
	if target == z.SOA.Serial {
		return &Batch{}, nil
	}
	if !target.Comparable(z.SOA.Serial) || !target.Before(z.SOA.Serial) {
		return nil, fmt.Errorf(
			"%w: the zone %s is at serial %s, and %s is not a state it has moved on from",
			zone.ErrInvalid, z.Name, z.SOA.Serial, target)
	}

	since, err := commitsBetween(ctx, tx, z, target, z.SOA.Serial)
	if err != nil {
		return nil, err
	}

	cs := &changeSet{}
	ch := cs.in(z)
	if uerr := a.undo(ctx, tx, ch, z, since); uerr != nil {
		return nil, uerr
	}
	if xerr := a.expandReverse(ctx, tx, cs, z, res); xerr != nil {
		return nil, xerr
	}
	if cs.empty() {
		// Everything the range did has already been undone by hand, or touched
		// only records the automation owns. Either way the zone already holds
		// the state that was asked for, and a serial step is how a zone tells
		// every secondary in the world to come and fetch a copy.
		return &Batch{}, nil
	}
	return a.commitAs(ctx, tx, cs, z, journal.KindRollback, &target, meta, res)
}

// commitsBetween returns the commits that took the zone from one serial to
// another, oldest first.
//
// The range is followed along the chain of serials rather than taken in listing
// order. One commit advances the serial by exactly one step (docs/decisions/
// D2), so SerialFrom and SerialTo say which commit followed which; the listing
// orders by the millisecond a commit was recorded and then by identifier, which
// for two commits inside one millisecond is no order at all.
func commitsBetween(
	ctx context.Context, r store.Reader, z *zone.Zone, from, to zone.Serial,
) ([]*journal.Commit, error) {
	f := store.CommitFilter{
		ZoneID: z.ID,
		Paging: store.Paging{Limit: store.MaxLimit},
	}

	ends := make(map[zone.Serial]*journal.Commit)
	for {
		page, err := r.ListCommits(ctx, f)
		if err != nil {
			return nil, err
		}
		for _, c := range page.Items {
			// The listing is newest first, so where a serial has come round
			// again the recent commit is the one kept.
			if _, seen := ends[c.SerialTo]; !seen {
				ends[c.SerialTo] = c
			}
		}
		if chain, ok := chainBack(ends, from, to); ok {
			return withEvents(ctx, r, chain)
		}
		if page.NextCursor == "" {
			return nil, fmt.Errorf(
				"%w: the history of %s does not run from serial %s to serial %s",
				store.ErrNotFound, z.Name, from, to)
		}
		f.Cursor = page.NextCursor
	}
}

// chainBack follows the serials from one end of the range to the other, and
// reports false where a step is missing from what has been read so far.
func chainBack(
	ends map[zone.Serial]*journal.Commit, from, to zone.Serial,
) ([]*journal.Commit, bool) {
	var out []*journal.Commit
	for at := to; at != from; {
		c, ok := ends[at]
		if !ok {
			return nil, false
		}
		out = append(out, c)
		at = c.SerialFrom
		// A serial that has come all the way round can point at itself through
		// the map, and following that would not end.
		if len(out) > len(ends) {
			return nil, false
		}
	}
	reverse(out)
	return out, true
}

// withEvents reads each commit of the chain in full. A listing carries no
// events, and the events are the whole point of reading a range.
func withEvents(
	ctx context.Context, r store.Reader, chain []*journal.Commit,
) ([]*journal.Commit, error) {
	out := make([]*journal.Commit, len(chain))
	for i, c := range chain {
		full, err := r.CommitByID(ctx, c.ID)
		if err != nil {
			return nil, err
		}
		out[i] = full
	}
	return out, nil
}

// reverse turns a newest-first listing into an oldest-first one.
func reverse(cs []*journal.Commit) {
	for i, j := 0, len(cs)-1; i < j; i, j = i+1, j-1 {
		cs[i], cs[j] = cs[j], cs[i]
	}
}

// rdataKey identifies a resource record by everything that makes it that record
// and nothing else. The TTL is deliberately not part of it: two records with
// the same owner, class, type and data are the same record answering with a
// different lifetime, and the journal expresses a TTL change as a removal and
// an addition of that same record.
type rdataKey struct {
	name  zone.Name
	class zone.Class
	typ   zone.RRType
	rdata string
}

// undo works out what the zone held at the target and queues the difference.
func (a *Applier) undo(
	ctx context.Context, tx store.Tx, ch *changes, z *zone.Zone, since []*journal.Commit,
) error {
	at := make(map[rdataKey]*journal.Event)
	var soaAt *journal.Event

	for _, c := range since {
		for i := range c.Events {
			e := &c.Events[i]
			if e.Type == zone.TypeSOA {
				// The zone's own parameters, which are not a record. The first
				// removal in the range carries them as they stood at the
				// target; a zone cannot be created inside a range that starts
				// at a state it already had, so there is always one.
				if soaAt == nil && e.Op == journal.OpDel {
					soaAt = e
				}
				continue
			}
			k := rdataKey{name: e.Name, class: e.Class, typ: e.Type, rdata: e.RData.String()}
			if _, seen := at[k]; !seen {
				at[k] = e
			}
		}
	}

	if err := a.undoSOA(ch, z, soaAt); err != nil {
		return err
	}

	// Read once per owner name rather than once per record: an RRset that lost
	// half its members is one lookup, not five.
	current := make(map[zone.Name]map[rdataKey]*zone.Record)
	for k := range at {
		if _, done := current[k.name]; done {
			continue
		}
		recs, err := ownerRecords(ctx, tx, z.ID, k.name)
		if err != nil {
			return err
		}
		byData := make(map[rdataKey]*zone.Record, len(recs))
		for i := range recs {
			r := &recs[i]
			byData[rdataKey{
				name: r.Name, class: r.Class, typ: r.Type, rdata: r.RData.String(),
			}] = r
		}
		current[k.name] = byData
	}

	for k, e := range at {
		if err := undoOne(ch, z, k, e, current[k.name][k]); err != nil {
			return err
		}
	}
	return nil
}

// undoSOA queues the zone's own parameters as they stood at the target.
func (a *Applier) undoSOA(ch *changes, z *zone.Zone, soaAt *journal.Event) error {
	if soaAt == nil {
		return nil
	}
	was, err := zone.ParseSOAData(soaAt.RData.String())
	if err != nil {
		return fmt.Errorf("the start of authority recorded for %q: %w", z.Name, err)
	}
	// The record's own TTL is not part of the data, so it comes off the event.
	was.TTL = soaAt.TTL
	// The serial belongs to the commit doing the restoring, not to the state
	// being restored: moving forward is the whole point.
	was.Serial = z.SOA.Serial
	if sameSOA(z.SOA, was) {
		return nil
	}
	ch.soa = &soaChange{before: z.SOA, after: was}
	return nil
}

// undoOne queues whatever one record needs to be back as it was.
func undoOne(ch *changes, z *zone.Zone, k rdataKey, e *journal.Event, cur *zone.Record) error {
	// A record the automation owns is not this rollback's to move. It follows
	// from the record it was generated from, and expandReverse puts it where
	// that one now says it belongs.
	if cur != nil && cur.IsManaged() {
		return nil
	}

	if e.Op == journal.OpAdd {
		// It arrived inside the range, so at the target it was not there.
		if cur != nil {
			ch.remove(cur)
		}
		return nil
	}

	// It was removed inside the range, so at the target it was there.
	if cur == nil {
		rec, err := zone.NewRecord(z.ID, k.name, k.class, k.typ, e.TTL, k.rdata)
		if err != nil {
			return fmt.Errorf("the %s record recorded at %q cannot be restored: %w",
				k.typ, k.name, err)
		}
		rec.ID = zone.RecordID(id.New())
		ch.insert(&rec)
		return nil
	}

	// Still there, but possibly not with the lifetime it had. A TTL change is
	// recorded as a removal and an addition of the same record, so the removal
	// this range starts with carries the TTL of the target.
	if cur.TTL != e.TTL {
		after := *cur
		after.TTL = e.TTL
		ch.update(cur, &after)
	}
	return nil
}
