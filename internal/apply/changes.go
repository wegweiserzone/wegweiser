package apply

import (
	"context"

	"github.com/wegweiserzone/wegweiser/internal/journal"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// changeSet groups pending changes by the zone they belong to.
type changeSet struct {
	zones  map[zone.ZoneID]*zone.Zone
	byZone map[zone.ZoneID]*changes
	// order is the order the zones were first touched, so that the commits of
	// one command come out the same way every time.
	order []zone.ZoneID

	// policy overrides the server's reverse conflict policy for this command.
	// Empty leaves it to the setting, which is every command but the one that
	// exists to take an entry away from the name holding it.
	policy Policy
}

// in returns the pending changes for a zone, starting them if this is the first
// change to reach it.
func (cs *changeSet) in(z *zone.Zone) *changes {
	if cs.byZone == nil {
		cs.byZone = make(map[zone.ZoneID]*changes, 2)
		cs.zones = make(map[zone.ZoneID]*zone.Zone, 2)
	}
	c, ok := cs.byZone[z.ID]
	if !ok {
		c = &changes{}
		cs.byZone[z.ID] = c
		cs.zones[z.ID] = z
		cs.order = append(cs.order, z.ID)
	}
	return c
}

// pending returns the changes already queued for a zone, or nil.
func (cs *changeSet) pending(zid zone.ZoneID) *changes { return cs.byZone[zid] }

func (cs *changeSet) empty() bool {
	for _, c := range cs.byZone {
		if !c.empty() {
			return false
		}
	}
	return true
}

// changes is the concrete record-level work queued for one zone.
type changes struct {
	deletes []zone.Record
	updates []recordUpdate
	inserts []zone.Record

	// claims are records that do not change and are to generate their reverse
	// entry anyway, taking it from whatever holds it. It is how somebody says
	// "this is the canonical name for that address" (D3, D33).
	claims []zone.Record

	// touched are the owner names to re-check once the writes are in. An update
	// that moves a record contributes both the name it left and the one it
	// arrived at.
	touched map[zone.Name]struct{}

	// soa is a change to the zone's own start of authority. It is not a record
	// (data model §4.1) but it travels through the journal like one,
	// because that is where the history of the zone lives.
	soa *soaChange
}

// soaChange is the start of authority as it was and as it should become. The
// serial is not part of it: that belongs to the commit doing the restoring,
// not to the state being restored.
type soaChange struct {
	before zone.SOA
	after  zone.SOA
}

// recordUpdate replaces a record's content while keeping its identity.
type recordUpdate struct {
	before zone.Record
	after  zone.Record
}

func (c *changes) remove(r *zone.Record) {
	c.deletes = append(c.deletes, *r)
	c.touch(r.Name)
}

func (c *changes) update(before, after *zone.Record) {
	c.updates = append(c.updates, recordUpdate{before: *before, after: *after})
	c.touch(before.Name)
	c.touch(after.Name)
}

func (c *changes) insert(r *zone.Record) {
	c.inserts = append(c.inserts, *r)
	c.touch(r.Name)
}

// claim queues a record to generate its reverse entry without changing the
// record itself.
func (c *changes) claim(r *zone.Record) { c.claims = append(c.claims, *r) }

func (c *changes) touch(n zone.Name) {
	if c.touched == nil {
		c.touched = make(map[zone.Name]struct{})
	}
	c.touched[n] = struct{}{}
}

// empty reports whether the command amounted to nothing for this zone.
func (c *changes) empty() bool {
	return c.soa == nil &&
		len(c.deletes) == 0 && len(c.updates) == 0 && len(c.inserts) == 0
}

// removing reports whether a record is already queued for removal, so that a
// later step does not take a row that is on its way out for one that is staying.
func (c *changes) removing(rid zone.RecordID) bool {
	if c == nil {
		return false
	}
	for i := range c.deletes {
		if c.deletes[i].ID == rid {
			return true
		}
	}
	return false
}

// arriving returns the records this change set will leave in an RRset that were
// not there before: additions, and updates that moved into it.
func (c *changes) arriving(key zone.RRsetKey) []zone.Record {
	if c == nil {
		return nil
	}
	var out []zone.Record
	for i := range c.inserts {
		if c.inserts[i].Key() == key {
			out = append(out, c.inserts[i])
		}
	}
	for i := range c.updates {
		if c.updates[i].after.Key() == key {
			out = append(out, c.updates[i].after)
		}
	}
	return out
}

// arrivingAt returns the records this change set will leave at an owner name
// that were not there before, whatever their type.
func (c *changes) arrivingAt(name zone.Name) []zone.Record {
	if c == nil {
		return nil
	}
	var out []zone.Record
	for i := range c.inserts {
		if c.inserts[i].Name.Equal(name) {
			out = append(out, c.inserts[i])
		}
	}
	for i := range c.updates {
		if c.updates[i].after.Name.Equal(name) {
			out = append(out, c.updates[i].after)
		}
	}
	return out
}

// write carries out every zone's changes, in the one order that works.
//
// Removals come before everything else, across all zones: an RRset being
// replaced can hold a record whose data is about to reappear under a different
// identity, and the index that forbids a duplicate resource record (RFC 2181
// §5) would refuse the new one while the old one is still there.
func (cs *changeSet) write(ctx context.Context, tx store.Tx) error {
	for _, d := range cs.ordered(func(c *changes) []zone.Record { return c.deletes }, true) {
		if err := tx.DeleteRecord(ctx, d.ID); err != nil {
			return err
		}
	}

	for _, zid := range cs.order {
		c := cs.byZone[zid]
		for i := range c.updates {
			if err := tx.UpdateRecord(ctx, &c.updates[i].after); err != nil {
				return err
			}
		}
	}

	// Additions run the other way round: a generated record carries a reference
	// to the record it came from, and that one has to be there first.
	for _, r := range cs.ordered(func(c *changes) []zone.Record { return c.inserts }, false) {
		if err := tx.InsertRecord(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

// ordered gathers one kind of change from every zone and puts it in the order
// the provenance links require.
//
// Removals go bottom-up: the database takes a generated record away as a side
// effect of removing its source, so removing the source first would leave this
// package deleting a row that is already gone. Additions go top-down: a
// generated record refers to the record it came from, which has to exist by
// then. Either chain can be more than one link long (an RFC 2317 delegation
// hangs off the entry it delegates to, which hangs off the address record) so
// the order is worked out from the links rather than assumed.
func (cs *changeSet) ordered(pick func(*changes) []zone.Record, dependentsFirst bool) []*zone.Record {
	var all []*zone.Record
	for _, zid := range cs.order {
		records := pick(cs.byZone[zid])
		for i := range records {
			all = append(all, &records[i])
		}
	}

	// waiting counts, for each record, what still has to happen before it can.
	waiting := make(map[zone.RecordID]int, len(all))
	queued := make(map[zone.RecordID]struct{}, len(all))
	for _, r := range all {
		queued[r.ID] = struct{}{}
	}
	for _, r := range all {
		if r.ManagedBy == "" {
			continue
		}
		if _, ours := queued[r.ManagedBy]; !ours {
			continue
		}
		if dependentsFirst {
			waiting[r.ManagedBy]++ // the source waits for what hangs off it
		} else {
			waiting[r.ID]++ // the derived record waits for its source
		}
	}

	out := make([]*zone.Record, 0, len(all))
	for len(all) > 0 {
		var held []*zone.Record
		for _, r := range all {
			if waiting[r.ID] > 0 {
				held = append(held, r)
				continue
			}
			out = append(out, r)
			if r.ManagedBy == "" {
				continue
			}
			if dependentsFirst {
				waiting[r.ManagedBy]--
			} else {
				for _, other := range all {
					if other.ManagedBy == r.ID {
						waiting[other.ID]--
					}
				}
			}
		}
		if len(held) == len(all) {
			// Provenance is a forest, so this cannot happen; refusing to loop
			// forever is cheaper than proving it here.
			return append(out, held...)
		}
		all = held
	}
	return out
}

// events renders the changes as the journal records them: every removal, then
// every addition, numbered from zero.
//
// That order is not presentation. RFC 1995 §2 frames an incremental transfer as
// the deletions of a step followed by its additions, so a commit written this
// way is already a difference sequence and nothing has to be sorted at transfer
// time. A modification is the deletion of the old record and the addition of
// the new one, which is the only way the wire can express it.
func (c *changes) events() []journal.Event {
	out := make([]journal.Event, 0, len(c.deletes)+2*len(c.updates)+len(c.inserts))

	for i := range c.deletes {
		out = append(out, eventFor(journal.OpDel, &c.deletes[i]))
	}
	for i := range c.updates {
		out = append(out, eventFor(journal.OpDel, &c.updates[i].before))
	}
	for i := range c.updates {
		out = append(out, eventFor(journal.OpAdd, &c.updates[i].after))
	}
	for i := range c.inserts {
		out = append(out, eventFor(journal.OpAdd, &c.inserts[i]))
	}

	for i := range out {
		out[i].Seq = i
	}
	return out
}

func eventFor(op journal.Op, r *zone.Record) journal.Event {
	return journal.Event{
		Op:    op,
		Name:  r.Name,
		Class: r.Class,
		Type:  r.Type,
		TTL:   r.TTL,
		RData: r.RData,
	}
}
