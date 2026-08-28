package apply

import (
	"context"
	"fmt"

	"github.com/wegweiserzone/wegweiser/internal/id"
	"github.com/wegweiserzone/wegweiser/internal/journal"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// Meta is who asked for a change and why. Every commit carries it, because an
// audit log that cannot say who did something is a list of mysteries.
type Meta struct {
	Source  journal.Source
	Actor   string
	Comment string
}

// Validate reports whether the metadata is usable.
func (m Meta) Validate() error {
	if !m.Source.Valid() {
		return fmt.Errorf("%w commit source %q", zone.ErrInvalid, m.Source)
	}
	return nil
}

// CreateZone brings a zone into existence together with its first records, and
// records it as one commit.
//
// The zone has to be usable when the call returns: at least one NS record at
// the apex (RFC 1034 §4.2.1). A zone with none names no authority for itself
// and no parent can delegate to it, and refusing it here means every zone in
// the database is one that can actually be answered from. Supplying a sensible
// default, usually an NS pointing at the SOA's primary, is the job of
// whatever is talking to the person, not of the write path.
//
// The zone's own serial is where the journal starts counting. An import
// therefore keeps the serial the zone already had elsewhere, which its
// secondaries have seen; see docs/decisions/ D2.
func (a *Applier) CreateZone(
	ctx context.Context, z *zone.Zone, records []zone.Record, meta Meta,
) (*Result, error) {
	if z == nil {
		return nil, fmt.Errorf("%w: no zone given", zone.ErrInvalid)
	}
	// Minted here rather than in the plan, because it is the key the lock has
	// to be taken on before any of this reads the store.
	if z.ID == "" {
		z.ID = zone.ZoneID(id.New())
	}

	unlock := a.locks.lock(string(z.ID))
	defer unlock()

	b, res, err := a.PlanCreateZone(ctx, z, records, meta)
	if err != nil {
		return nil, err
	}
	if err := a.ApplyBatch(ctx, b); err != nil {
		return nil, err
	}
	return res, nil
}

// PlanCreateZone works out what bringing a zone into being would amount to,
// and leaves the store as it found it. See [Applier.Plan].
func (a *Applier) PlanCreateZone(
	ctx context.Context, z *zone.Zone, records []zone.Record, meta Meta,
) (*Batch, *Result, error) {
	if z == nil {
		return nil, nil, fmt.Errorf("%w: no zone given", zone.ErrInvalid)
	}
	if err := meta.Validate(); err != nil {
		return nil, nil, err
	}
	if err := z.Validate(); err != nil {
		return nil, nil, err
	}
	if z.SOA.Serial.IsZero() {
		return nil, nil, fmt.Errorf("%w: a zone starts at a non-zero serial (RFC 1912 §2.2)",
			zone.ErrInvalid)
	}

	// Before the transaction, so that what happens is settled before it starts.
	if z.ID == "" {
		z.ID = zone.ZoneID(id.New())
	}
	for i := range records {
		mintID(&records[i], z.ID)
	}
	if err := zone.ValidateZone(*z, records); err != nil {
		return nil, nil, err
	}

	res := &Result{}
	b, err := a.planning(ctx, func(tx store.Tx) (*Batch, error) {
		return a.createZoneIn(ctx, tx, z, records, meta, res)
	})
	if err != nil {
		return nil, nil, err
	}
	return b, res, nil
}

func (a *Applier) createZoneIn(
	ctx context.Context, tx store.Tx, z *zone.Zone, records []zone.Record, meta Meta, res *Result,
) (*Batch, error) {
	cs := &changeSet{}
	ch := cs.in(z)
	for i := range records {
		ch.insert(&records[i])
	}
	// The zone is not in the store yet, so the automation works from the
	// queued records. RFC 1034 §4.2.1 means there is always at least one.
	if err := a.expandReverse(ctx, tx, cs, z, res); err != nil {
		return nil, err
	}

	b, err := a.plan(cs, z, journal.KindZoneCreate, nil, meta)
	if err != nil {
		return nil, err
	}
	b.Zones = []ZoneOp{{Kind: ZoneCreate, ZoneID: z.ID, Zone: z}}

	if werr := b.planned(ctx, tx); werr != nil {
		return nil, werr
	}
	res.Commits = append(res.Commits, b.Commits...)
	return b, nil
}

// UpdateZone replaces a zone's own settings, leaving its records alone.
//
// It returns nil when nothing changed. Where something did, the serial advances
// like any other change: a zone's settings include the SOA timers, which are
// served, and whether the zone answers at all.
func (a *Applier) UpdateZone(
	ctx context.Context, z *zone.Zone, meta Meta,
) (*Result, error) {
	if z == nil {
		return nil, fmt.Errorf("%w: no zone given", zone.ErrInvalid)
	}
	if err := meta.Validate(); err != nil {
		return nil, err
	}
	if err := z.Validate(); err != nil {
		return nil, err
	}

	unlock := a.locks.lock(string(z.ID))
	defer unlock()

	b, res, err := a.PlanUpdateZone(ctx, z, meta)
	if err != nil {
		return nil, err
	}
	if err := a.ApplyBatch(ctx, b); err != nil {
		return nil, err
	}
	return res, nil
}

// PlanUpdateZone works out what changing a zone's settings would amount to,
// and leaves the store as it found it. See [Applier.Plan].
func (a *Applier) PlanUpdateZone(
	ctx context.Context, z *zone.Zone, meta Meta,
) (*Batch, *Result, error) {
	if z == nil {
		return nil, nil, fmt.Errorf("%w: no zone given", zone.ErrInvalid)
	}
	if err := meta.Validate(); err != nil {
		return nil, nil, err
	}
	if err := z.Validate(); err != nil {
		return nil, nil, err
	}

	res := &Result{}
	b, err := a.planning(ctx, func(tx store.Tx) (*Batch, error) {
		return a.updateZoneIn(ctx, tx, z, meta, res)
	})
	if err != nil {
		return nil, nil, err
	}
	return b, res, nil
}

func (a *Applier) updateZoneIn(
	ctx context.Context, tx store.Tx, z *zone.Zone, meta Meta, res *Result,
) (*Batch, error) {
	before, err := tx.ZoneByID(ctx, z.ID)
	if err != nil {
		return nil, err
	}
	if !before.Name.Equal(z.Name) {
		// Renaming a zone is creating a different one: every record's owner
		// name, every sort key and the whole journal belong to the old name.
		return nil, fmt.Errorf(
			"%w: a zone cannot be renamed from %q to %q; create the new zone and move the "+
				"records across", zone.ErrInvalid, before.Name, z.Name)
	}

	// The serial is the journal's, not the caller's, whatever was handed in.
	next := before.SOA.Serial.Next()
	z.SOA.Serial = next
	if sameZoneSettings(before, z) {
		z.SOA.Serial = before.SOA.Serial
		return &Batch{}, nil
	}

	// Only a change to the start of authority itself is a record change.
	// Comments, the default TTL and whether reverse records are generated are
	// all invisible on the wire.
	var events []journal.Event
	if !sameSOA(before.SOA, z.SOA) {
		was, werr := soaEvent(journal.OpDel, before)
		if werr != nil {
			return nil, werr
		}
		now, nerr := soaEvent(journal.OpAdd, z)
		if nerr != nil {
			return nil, nerr
		}
		was.Seq, now.Seq = 0, 1
		events = []journal.Event{was, now}
	}

	b := &Batch{
		set:   &changeSet{},
		Zones: []ZoneOp{{Kind: ZoneUpdate, ZoneID: z.ID, Zone: z}},
		Commits: []*journal.Commit{{
			ID:         journal.CommitID(id.New()),
			ZoneID:     z.ID,
			ZoneName:   z.Name,
			SerialFrom: before.SOA.Serial,
			SerialTo:   next,
			Kind:       journal.KindZoneUpdate,
			Source:     meta.Source,
			Actor:      meta.Actor,
			Comment:    meta.Comment,
			Events:     events,
			CreatedAt:  a.now(),
		}},
	}
	if werr := b.planned(ctx, tx); werr != nil {
		return nil, werr
	}
	res.Commits = append(res.Commits, b.Commits...)
	return b, nil
}

// DeleteZone removes a zone and everything in it, and records that it happened.
func (a *Applier) DeleteZone(
	ctx context.Context, zid zone.ZoneID, meta Meta,
) (*Result, error) {
	unlock := a.locks.lock(string(zid))
	defer unlock()

	b, res, err := a.PlanDeleteZone(ctx, zid, meta)
	if err != nil {
		return nil, err
	}
	if err := a.ApplyBatch(ctx, b); err != nil {
		return nil, err
	}
	return res, nil
}

// PlanDeleteZone works out what removing a zone would amount to, and leaves
// the store as it found it. See [Applier.Plan].
func (a *Applier) PlanDeleteZone(
	ctx context.Context, zid zone.ZoneID, meta Meta,
) (*Batch, *Result, error) {
	if err := meta.Validate(); err != nil {
		return nil, nil, err
	}

	res := &Result{}
	b, err := a.planning(ctx, func(tx store.Tx) (*Batch, error) {
		return a.deleteZoneIn(ctx, tx, zid, meta, res)
	})
	if err != nil {
		return nil, nil, err
	}
	return b, res, nil
}

func (a *Applier) deleteZoneIn(
	ctx context.Context, tx store.Tx, zid zone.ZoneID, meta Meta, res *Result,
) (*Batch, error) {
	z, err := tx.ZoneByID(ctx, zid)
	if err != nil {
		return nil, err
	}

	// Whatever this zone's records generated elsewhere goes with them. The
	// database would cascade it away regardless, but the zones holding those
	// entries have journals of their own, and a row that vanishes without an
	// event is a change they never saw.
	cs := &changeSet{}
	for rec, ierr := range tx.ManagedByZone(ctx, zid) {
		if ierr != nil {
			return nil, ierr
		}
		other, zerr := a.zoneOf(ctx, tx, cs, rec.ZoneID)
		if zerr != nil {
			return nil, zerr
		}
		cs.in(other).remove(rec)
	}

	b, err := a.plan(cs, z, journal.KindEdit, nil, meta)
	if err != nil {
		return nil, err
	}

	// The zone's own commit leads: it is the change that was asked for, and
	// what follows is its consequences elsewhere. Both travel together, so a
	// node cannot record the departure without recording what left with it.
	b.Commits = append([]*journal.Commit{{
		ID:         journal.CommitID(id.New()),
		ZoneID:     z.ID,
		ZoneName:   z.Name,
		SerialFrom: z.SOA.Serial,
		SerialTo:   z.SOA.Serial.Next(),
		Kind:       journal.KindZoneDelete,
		Source:     meta.Source,
		Actor:      meta.Actor,
		Comment:    meta.Comment,
		CreatedAt:  a.now(),
	}}, b.Commits...)
	b.Zones = []ZoneOp{{Kind: ZoneDelete, ZoneID: zid}}

	if werr := b.planned(ctx, tx); werr != nil {
		return nil, werr
	}
	res.Commits = append(res.Commits, b.Commits...)
	return b, nil
}

// sameZoneSettings reports whether two zones differ in anything a caller can
// set. The serial is excluded because the journal owns it, and so are the
// timestamps, which the store owns.
func sameZoneSettings(a, b *zone.Zone) bool {
	return a.Name.Equal(b.Name) &&
		a.Kind == b.Kind && a.Prefix == b.Prefix &&
		sameSOA(a.SOA, b.SOA) &&
		a.DefaultTTL == b.DefaultTTL &&
		sameOptionalBool(a.AutoReverse, b.AutoReverse) &&
		a.Disabled == b.Disabled && a.Comment == b.Comment
}

// sameSOA compares two starts of authority by everything except the serial,
// which belongs to the journal rather than to whoever is editing the zone.
func sameSOA(a, b zone.SOA) bool {
	a.Serial, b.Serial = zone.Serial{}, zone.Serial{}
	return a == b
}

func sameOptionalBool(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// soaEvent renders a zone's start of authority as a journal event.
//
// The SOA is stored as columns rather than as a record (data model
// §3.1), so there is no row to read it from, but its history belongs in the
// journal like anything else that is served, and RFC 1995 frames a transfer
// with exactly this record.
func soaEvent(op journal.Op, z *zone.Zone) (journal.Event, error) {
	return soaEventFor(op, z.Name, z.SOA)
}

// soaEventFor is soaEvent for a start of authority that is not the one the
// zone currently holds, which is what restoring an earlier state produces.
func soaEventFor(op journal.Op, name zone.Name, soa zone.SOA) (journal.Event, error) {
	// Parsed rather than asserted. A validated SOA always renders to data that
	// parses, but this is the write path, and a panic there would take the
	// server down over a record it could simply have refused.
	data, err := zone.ParseRData(zone.TypeSOA, zone.ClassIN, soa.RData())
	if err != nil {
		return journal.Event{}, fmt.Errorf(
			"%w: the start of authority of %q does not render to usable record data: %w",
			zone.ErrInvalid, name, err)
	}
	return journal.Event{
		Op:    op,
		Name:  name,
		Class: zone.ClassIN,
		Type:  zone.TypeSOA,
		TTL:   soa.TTL,
		RData: data,
	}, nil
}
