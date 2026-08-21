package apply

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/id"
	"github.com/wegweiserzone/wegweiser/internal/journal"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// Applier is the only thing that changes zone data.
//
// Every write goes through it, and every write it makes is recorded as a commit
// in the same transaction (architecture invariant 4). Nothing else advances a
// zone serial. That is what lets the audit log, the diff view, rollback and
// incremental transfer all read from one structure instead of four.
type Applier struct {
	store store.Store
	now   func() time.Time

	autoReverseDefault bool
	policy             Policy

	locks keyedMutex
}

// Options configure an applier.
type Options struct {
	// Now supplies the time a commit is stamped with. Nil uses [time.Now];
	// tests pass their own so that the stamp is theirs to predict.
	Now func() time.Time

	// AutoReverse is whether reverse entries are generated for zones that do
	// not say either way. Nil means on, so a caller that does not mention the
	// automation gets it: it is the product's headline feature, and a zero
	// value that switched it off would turn every forgotten option into a
	// silently half-working server. Only a caller that means to switch it off
	// writes it down. A zone overrides this on itself.
	AutoReverse *bool

	// Policy is what to do when an address already answers a reverse lookup
	// with another name. Empty means [PolicyFirstWins].
	Policy Policy
}

// New returns an applier writing through s.
func New(s store.Store, opts Options) (*Applier, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Policy == "" {
		opts.Policy = PolicyFirstWins
	}
	if !opts.Policy.Valid() {
		return nil, fmt.Errorf("%w reverse policy %q", zone.ErrInvalid, opts.Policy)
	}
	autoReverse := true
	if opts.AutoReverse != nil {
		autoReverse = *opts.AutoReverse
	}
	return &Applier{
		store:              s,
		now:                opts.Now,
		autoReverseDefault: autoReverse,
		policy:             opts.Policy,
	}, nil
}

// Result is what a change produced.
//
// Conflicts and missing zones are returned rather than logged, because both are
// things a person has to decide about and neither is an error: a conflict means
// the address already answers with another name, and a missing zone means there
// is nowhere to put a reverse entry. See docs/decisions.md D3 and D6.
type Result struct {
	// Commits are the commits written, one per zone the change reached, in the
	// order the zones were touched. It is empty when nothing changed.
	Commits []*journal.Commit

	Conflicts    []Conflict
	MissingZones []MissingZone

	// Skipped are records an import left out because the zone could never
	// answer with them. Empty for every other kind of change.
	Skipped []Skipped
}

// Changed reports whether anything was written.
func (r *Result) Changed() bool { return r != nil && len(r.Commits) > 0 }

// Commit returns the commit for the zone the change was addressed to, or nil.
func (r *Result) Commit() *journal.Commit {
	if !r.Changed() {
		return nil
	}
	return r.Commits[0]
}

// Apply carries out a command.
func (a *Applier) Apply(ctx context.Context, cmd Command) (*Result, error) {
	if err := cmd.Validate(); err != nil {
		return nil, err
	}
	normalize(&cmd)

	// One command per zone at a time. SQLite serializes writers anyway and the
	// unique index on (zone_id, serial_to) is the last line of defence, but a
	// database that does allow concurrent writers would otherwise have two
	// commands read the same serial and compute the same successor: one of
	// them then failing on a constraint after doing all its work.
	unlock := a.locks.lock(string(cmd.ZoneID))
	defer unlock()

	res := &Result{}
	err := a.store.Update(ctx, func(tx store.Tx) error {
		res.Commits, res.Conflicts, res.MissingZones = nil, nil, nil
		return a.applyIn(ctx, tx, cmd, res)
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// normalize fills in everything the command leaves undetermined.
func normalize(cmd *Command) {
	for i := range cmd.Ops {
		op := &cmd.Ops[i]
		switch op.Action {
		case ActionAdd:
			mintID(op.Record, cmd.ZoneID)
		case ActionUpdate:
			op.Record.ID = op.RecordID
			op.Record.ZoneID = cmd.ZoneID
		case ActionReplaceRRset:
			for j := range op.Records {
				mintID(&op.Records[j], cmd.ZoneID)
			}
		case ActionDelete, ActionDetach:
			// Addressed by identifier, so there is nothing left to determine.
		}
	}
}

func mintID(r *zone.Record, zid zone.ZoneID) {
	if r == nil {
		return
	}
	if r.ID == "" {
		r.ID = zone.RecordID(id.New())
	}
	r.ZoneID = zid
}

// applyIn does the work inside one transaction.
func (a *Applier) applyIn(ctx context.Context, tx store.Tx, cmd Command, res *Result) error {
	z, err := tx.ZoneByID(ctx, cmd.ZoneID)
	if err != nil {
		return err
	}
	if cmd.ExpectedSerial != nil && *cmd.ExpectedSerial != z.SOA.Serial {
		return fmt.Errorf(
			"%w: the zone %s was at serial %s when this change was prepared and is now at %s; "+
				"reload it and try again", store.ErrConflict, z.Name, cmd.ExpectedSerial, z.SOA.Serial)
	}

	var cs changeSet
	cs.in(z) // the addressed zone commits first, even if only other zones change
	for i := range cmd.Ops {
		if err := a.resolve(ctx, tx, cs.in(z), z, &cmd.Ops[i]); err != nil {
			return fmt.Errorf("operation %d: %w", i, err)
		}
	}
	if err := a.expandReverse(ctx, tx, &cs, z, res); err != nil {
		return err
	}
	if cs.empty() {
		return nil
	}
	return a.commit(ctx, tx, &cs, z, cmd.Kind, Meta{
		Source: cmd.Source, Actor: cmd.Actor, Comment: cmd.Comment,
	}, res)
}

// commit writes each zone's changes and records them, one commit per zone.
//
// The zone the change was addressed to keeps the caller's kind and metadata.
// Every other zone is one the automation reached on its own, so its commit says
// so: the person did not edit that zone, the server did, on their behalf.
func (a *Applier) commit(
	ctx context.Context, tx store.Tx, cs *changeSet, primary *zone.Zone,
	kind journal.Kind, meta Meta, res *Result,
) error {
	return a.commitAs(ctx, tx, cs, primary, kind, nil, meta, res)
}

// commitAs is commit for a change that names a serial it restores, which only
// a rollback does. The schema allows the target on a rollback and on nothing
// else, so it is a parameter here rather than a field on the metadata every
// caller would then have to leave empty.
func (a *Applier) commitAs(
	ctx context.Context, tx store.Tx, cs *changeSet, primary *zone.Zone,
	kind journal.Kind, revertsTo *zone.Serial, meta Meta, res *Result,
) error {
	if err := cs.write(ctx, tx); err != nil {
		return err
	}

	for _, zid := range cs.order {
		ch := cs.byZone[zid]
		if ch.empty() {
			continue
		}
		z := cs.zones[zid]

		// The zone as it now stands is what gets validated, read back from the
		// transaction rather than simulated. A simulation is a second model of
		// the same thing, and the two drift.
		if err := validateTouched(ctx, tx, z, ch.touched); err != nil {
			return err
		}

		// A generated entry lands in a zone the person never named, and the
		// change to that zone is the server's own doing.
		zoneKind, zoneMeta := kind, meta
		if zid != primary.ID {
			zoneKind = journal.KindEdit
			zoneMeta.Source = journal.SourceSystem
			zoneMeta.Comment = "reverse entries kept in step with " + primary.Name.String()
		}

		commit := &journal.Commit{
			ID:         journal.CommitID(id.New()),
			ZoneID:     z.ID,
			ZoneName:   z.Name,
			SerialFrom: z.SOA.Serial,
			SerialTo:   z.SOA.Serial.Next(),
			Kind:       zoneKind,
			Source:     zoneMeta.Source,
			Actor:      zoneMeta.Actor,
			Comment:    zoneMeta.Comment,
			Events:     ch.events(),
			CreatedAt:  a.now(),
		}
		if zid == primary.ID {
			commit.RevertsTo = revertsTo
		}
		if err := a.writeSOA(ctx, tx, z, ch, commit); err != nil {
			return err
		}
		if err := tx.AppendCommit(ctx, commit); err != nil {
			return err
		}
		res.Commits = append(res.Commits, commit)
	}
	return nil
}

// writeSOA advances the zone's serial, restoring its start of authority along
// the way when the change carries one.
func (a *Applier) writeSOA(
	ctx context.Context, tx store.Tx, z *zone.Zone, ch *changes, commit *journal.Commit,
) error {
	if ch.soa == nil {
		return tx.SetZoneSerial(ctx, z.ID, commit.SerialTo)
	}

	restored := ch.soa.after
	restored.Serial = commit.SerialTo

	was, err := soaEventFor(journal.OpDel, z.Name, ch.soa.before)
	if err != nil {
		return err
	}
	now, err := soaEventFor(journal.OpAdd, z.Name, restored)
	if err != nil {
		return err
	}

	// The SOA leads each half rather than the whole list. A commit is a
	// difference sequence: every deletion comes before every addition (RFC
	// 1995 §2), so putting the pair in front would leave an addition ahead of
	// the record deletions and produce a commit the journal refuses.
	adds := slices.IndexFunc(commit.Events, func(e journal.Event) bool {
		return e.Op == journal.OpAdd
	})
	if adds < 0 {
		adds = len(commit.Events)
	}
	events := make([]journal.Event, 0, len(commit.Events)+2)
	events = append(events, was)
	events = append(events, commit.Events[:adds]...)
	events = append(events, now)
	events = append(events, commit.Events[adds:]...)
	for i := range events {
		events[i].Seq = i
	}
	commit.Events = events

	next := *z
	next.SOA = restored
	if err := next.Validate(); err != nil {
		return fmt.Errorf("the restored start of authority of %q: %w", z.Name, err)
	}
	return tx.UpdateZone(ctx, &next)
}

// resolve turns one authored operation into concrete record changes.
func (a *Applier) resolve(
	ctx context.Context, tx store.Tx, ch *changes, z *zone.Zone, op *RecordOp,
) error {
	switch op.Action {
	case ActionAdd:
		if !z.Contains(op.Record.Name) {
			return outsideZone(z, op.Record.Name)
		}
		ch.insert(op.Record)
		return nil

	case ActionUpdate:
		before, err := tx.RecordByID(ctx, op.RecordID)
		if err != nil {
			return err
		}
		if err := checkOwnedAndAuthored(z, before); err != nil {
			return err
		}
		if !z.Contains(op.Record.Name) {
			return outsideZone(z, op.Record.Name)
		}
		// Provenance and the creation stamp belong to the record, not to the
		// edit, so an update that does not mention them keeps them.
		after := *op.Record
		after.ManagedBy, after.ManagedKind = before.ManagedBy, before.ManagedKind
		after.CreatedAt = before.CreatedAt
		if sameContent(before, &after) {
			return nil
		}
		ch.update(before, &after)
		return nil

	case ActionDelete:
		before, err := tx.RecordByID(ctx, op.RecordID)
		if err != nil {
			return err
		}
		if err := checkOwnedAndAuthored(z, before); err != nil {
			return err
		}
		ch.remove(before)
		return nil

	case ActionDetach:
		return a.resolveDetach(ctx, tx, ch, z, op)

	case ActionReplaceRRset:
		return a.resolveReplace(ctx, tx, ch, z, op)
	}
	return fmt.Errorf("%w operation %q", zone.ErrInvalid, op.Action)
}

// resolveDetach turns a generated record into an authored one.
//
// This is the way out of "you cannot edit a generated record": the record keeps
// its data and its identity and loses its link, and the automation stops
// touching it (D4). A flag that kept the link while overriding the value was
// rejected; it makes a third record state that every consumer has to
// understand, in exchange for a warning nobody reads.
func (a *Applier) resolveDetach(
	ctx context.Context, tx store.Tx, ch *changes, z *zone.Zone, op *RecordOp,
) error {
	before, err := tx.RecordByID(ctx, op.RecordID)
	if err != nil {
		return err
	}
	if before.ZoneID != z.ID {
		return fmt.Errorf("%w: the record %q belongs to another zone", zone.ErrInvalid, before.ID)
	}
	if !before.IsManaged() {
		// Not an error: asking for a record to be nobody's derivative when it
		// already is has got what it asked for.
		return nil
	}

	after := *before
	after.ManagedBy, after.ManagedKind = "", ""
	ch.update(before, &after)
	return nil
}

// resolveReplace works out the difference between what an RRset holds and what
// it is being asked to hold.
func (a *Applier) resolveReplace(
	ctx context.Context, tx store.Tx, ch *changes, z *zone.Zone, op *RecordOp,
) error {
	if !z.Contains(op.Key.Name) {
		return outsideZone(z, op.Key.Name)
	}

	existing, err := rrset(ctx, tx, z.ID, op.Key)
	if err != nil {
		return err
	}
	for i := range existing {
		if err := checkOwnedAndAuthored(z, &existing[i]); err != nil {
			return err
		}
	}

	// Data is canonical, so identity within the set is string equality.
	wanted := make(map[string]*zone.Record, len(op.Records))
	for i := range op.Records {
		wanted[op.Records[i].RData.String()] = &op.Records[i]
	}

	for i := range existing {
		before := &existing[i]
		after, kept := wanted[before.RData.String()]
		if !kept {
			ch.remove(before)
			continue
		}
		delete(wanted, before.RData.String())

		// Same data, so the record stays and only what is around it can differ.
		next := *after
		next.ID = before.ID
		next.ManagedBy, next.ManagedKind = before.ManagedBy, before.ManagedKind
		next.CreatedAt = before.CreatedAt
		if !sameContent(before, &next) {
			ch.update(before, &next)
		}
	}

	// Whatever is left was not there before. Added in the order the caller gave
	// them, so that a diff reads the way the request was written.
	for i := range op.Records {
		if r, ok := wanted[op.Records[i].RData.String()]; ok {
			ch.insert(r)
			delete(wanted, op.Records[i].RData.String())
		}
	}
	return nil
}

// checkOwnedAndAuthored refuses a record that belongs to another zone, and one
// that the server generated.
func checkOwnedAndAuthored(z *zone.Zone, r *zone.Record) error {
	if r.ZoneID != z.ID {
		return fmt.Errorf("%w: the record %q belongs to another zone", zone.ErrInvalid, r.ID)
	}
	if r.IsManaged() {
		// Editing the derived copy would either be undone at the next
		// reconciliation or leave the two disagreeing, and neither is what the
		// person asking wanted. Detaching is the way to take it over.
		return fmt.Errorf(
			"%w: the %s record at %q was generated from another record and is kept in step with "+
				"it; edit the record it came from, or detach this one to take it over",
			zone.ErrInvalid, r.Type, r.Name)
	}
	return nil
}

func outsideZone(z *zone.Zone, name zone.Name) error {
	return fmt.Errorf("%w: %q is not inside the zone %q", zone.ErrInvalid, name, z.Name)
}

// sameContent reports whether two records would answer a query identically.
// Identity, provenance and timestamps are deliberately not part of it.
func sameContent(a, b *zone.Record) bool {
	return a.Name.Equal(b.Name) &&
		a.Class == b.Class && a.Type == b.Type && a.TTL == b.TTL &&
		a.RData.Equal(b.RData) &&
		a.Comment == b.Comment && a.Disabled == b.Disabled
}

// validateTouched checks every owner name the command reached, as the zone now
// stands.
func validateTouched(ctx context.Context, tx store.Tx, z *zone.Zone, touched map[zone.Name]struct{}) error {
	names := make([]zone.Name, 0, len(touched))
	for n := range touched {
		names = append(names, n)
	}
	// Sorted so that a command breaking two names reports the same one every
	// time; a map would report whichever came up first.
	slices.SortFunc(names, func(a, b zone.Name) int { return a.Compare(b) })

	for _, name := range names {
		records, err := ownerRecords(ctx, tx, z.ID, name)
		if err != nil {
			return err
		}
		if err := zone.ValidateOwner(*z, name, records); err != nil {
			return err
		}

		// Where a delegation sits above this name, or at it, most types are
		// invisible: a query there is referred to the child and never answered
		// from here. The whole-zone check has always refused that and an
		// incremental write used not to, so the same end state was accepted or
		// refused depending on the order it was reached in.
		point, derr := closestDelegation(ctx, tx, z, name)
		if derr != nil {
			return derr
		}
		if verr := zone.ValidateUnderDelegation(name, records, point); verr != nil {
			return verr
		}
		if !z.IsApex(name) {
			continue
		}
		// RFC 1034 §4.2.1: a zone names its own authoritative servers. Losing
		// the last one leaves a zone no parent can delegate to, which no single
		// record can notice on its own.
		if !slices.ContainsFunc(records, func(r zone.Record) bool { return r.Type == zone.TypeNS }) {
			return fmt.Errorf(
				"%w: this would remove the last NS record at the apex of %q, and a zone must name "+
					"at least one authoritative server (RFC 1034 §4.2.1)", zone.ErrInvalid, z.Name)
		}
	}
	return nil
}

// closestDelegation returns the nearest name at or above name that this zone
// delegates away, or a zero name when there is none.
//
// NS at the apex is the zone naming its own servers (RFC 1034 §4.2.1) and is
// not a delegation, which is why the walk stops before it.
func closestDelegation(
	ctx context.Context, r store.Reader, z *zone.Zone, name zone.Name,
) (zone.Name, error) {
	for n := name; !z.IsApex(n); {
		page, err := r.ListRecords(ctx, store.RecordFilter{
			ZoneID: z.ID,
			Name:   n,
			Types:  []zone.RRType{zone.TypeNS},
		})
		if err != nil {
			return zone.Name{}, err
		}
		if len(page.Items) > 0 {
			return n, nil
		}
		parent, ok := n.Parent()
		if !ok {
			return zone.Name{}, nil
		}
		n = parent
	}
	return zone.Name{}, nil
}

// ownerRecords reads every record at one owner name.
func ownerRecords(ctx context.Context, r store.Reader, zid zone.ZoneID, name zone.Name) ([]zone.Record, error) {
	f := store.RecordFilter{
		ZoneID: zid,
		Name:   name,
		Paging: store.Paging{Limit: store.MaxLimit},
	}
	var out []zone.Record
	for {
		page, err := r.ListRecords(ctx, f)
		if err != nil {
			return nil, err
		}
		for _, rec := range page.Items {
			out = append(out, *rec)
		}
		if page.NextCursor == "" {
			return out, nil
		}
		f.Cursor = page.NextCursor
	}
}

// rrset reads the records forming one RRset.
func rrset(ctx context.Context, r store.Reader, zid zone.ZoneID, key zone.RRsetKey) ([]zone.Record, error) {
	all, err := ownerRecords(ctx, r, zid, key.Name)
	if err != nil {
		return nil, err
	}
	// The record filter selects by name and type; the class is narrowed here
	// because a zone in a class other than IN is a curiosity rather than a
	// reason for a column in every filter. Indexed rather than ranged by value:
	// a record is large enough that copying every one to look at three fields
	// is measurable on a zone being imported.
	out := all[:0]
	for i := range all {
		if all[i].Key() == key {
			out = append(out, all[i])
		}
	}
	return out, nil
}

// keyedMutex hands out one lock per key, and forgets a key once nobody holds
// it, so that a long-lived applier does not accumulate a mutex per zone that
// has ever been written.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

type keyedLock struct {
	mu       sync.Mutex
	refcount int
}

// lock blocks until the key is free and returns the function that releases it.
func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = make(map[string]*keyedLock)
	}
	l, ok := k.locks[key]
	if !ok {
		l = &keyedLock{}
		k.locks[key] = l
	}
	l.refcount++
	k.mu.Unlock()

	l.mu.Lock()

	return func() {
		l.mu.Unlock()

		k.mu.Lock()
		defer k.mu.Unlock()
		l.refcount--
		if l.refcount == 0 {
			delete(k.locks, key)
		}
	}
}
