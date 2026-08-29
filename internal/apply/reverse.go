package apply

import (
	"context"
	"errors"
	"fmt"
	"net/netip"

	"github.com/wegweiserzone/wegweiser/internal/id"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// Policy says what to do when an address already answers a reverse lookup with
// some other name. See docs/decisions/ D3.
type Policy string

const (
	// PolicyFirstWins keeps the reverse entry that is already there and reports
	// a conflict. It is the default, and the only setting that never changes an
	// answer nobody asked to change.
	PolicyFirstWins Policy = "first-wins"
	// PolicyLastWins replaces a generated entry with the new one, still
	// reporting the conflict. An entry someone wrote by hand is never replaced.
	PolicyLastWins Policy = "last-wins"
	// PolicyMulti keeps both. It is the most literal reading of "generate the
	// reverse entry", and it turns a routine change into a multi-record PTR set
	// that reverse-lookup checks are not built for.
	PolicyMulti Policy = "multi"
	// PolicyReject refuses the whole change.
	PolicyReject Policy = "reject"
)

// Valid reports whether p is one of the defined policies.
func (p Policy) Valid() bool {
	switch p {
	case PolicyFirstWins, PolicyLastWins, PolicyMulti, PolicyReject:
		return true
	default:
		return false
	}
}

// Conflict is a reverse entry that was not created because the address already
// answers with a different name.
//
// It is returned to the caller rather than logged: several names pointing at
// one address is the normal case (virtual hosts, a load balancer, a service
// alias) and a conflict only visible in the server log is the same as no
// conflict detection at all (D3).
type Conflict struct {
	// Source is the address record whose reverse entry was not created.
	Source zone.RecordID
	// SourceName is the name that record sits at, and Address what it points at.
	SourceName zone.Name
	Address    netip.Addr

	// ReverseZone and ReverseName are where the entry would have gone.
	ReverseZone zone.ZoneID
	ReverseName zone.Name

	// Existing is the name the address currently reverses to, and Generated
	// says whether the record holding it is one the server made. An entry
	// someone wrote by hand is never taken away from them.
	Existing  zone.Name
	Generated bool

	Policy Policy
}

// MissingZone reports that an address has no reverse zone to put an entry in.
//
// Creating one is an assertion of authority over a namespace, and doing that as
// a side effect of adding a record would be a surprise: for public address
// space it would be wrong. So the caller is told which zone would be needed and
// decides (D6).
type MissingZone struct {
	Source     zone.RecordID
	SourceName zone.Name
	Address    netip.Addr

	// Suggested is the reverse zone that would cover the address, at the
	// boundary such zones are conventionally delegated on: a /24 for IPv4 and a
	// /64 for IPv6.
	Suggested zone.Name
	Prefix    netip.Prefix
}

// autoReverse reports whether reverse entries are generated for a zone. A zone
// that does not say inherits the server's setting.
func (a *Applier) autoReverse(z *zone.Zone) bool {
	if z.AutoReverse != nil {
		return *z.AutoReverse
	}
	return a.autoReverseDefault
}

// expandReverse adds to the change set the reverse entries that the changes
// already in it imply.
func (a *Applier) expandReverse(
	ctx context.Context, tx store.Tx, cs *changeSet, src *zone.Zone, res *Result,
) error {
	if !a.autoReverse(src) {
		return nil
	}
	pending := cs.pending(src.ID)
	if pending == nil {
		return nil
	}

	// Removals first, and in full: a record whose address changed has to lose
	// the entry pointing at the old one before the new entry is placed, or the
	// first-wins check would find the departing entry and call it a conflict.
	for i := range pending.deletes {
		if err := a.retireGenerated(ctx, tx, cs, &pending.deletes[i]); err != nil {
			return err
		}
	}
	for i := range pending.updates {
		u := &pending.updates[i]
		if sameReverse(&u.before, &u.after) {
			continue
		}
		if err := a.retireGenerated(ctx, tx, cs, &u.before); err != nil {
			return err
		}
	}

	for i := range pending.updates {
		u := &pending.updates[i]
		if sameReverse(&u.before, &u.after) {
			continue
		}
		if err := a.generate(ctx, tx, cs, &u.after, res); err != nil {
			return err
		}
	}
	for i := range pending.inserts {
		if err := a.generate(ctx, tx, cs, &pending.inserts[i], res); err != nil {
			return err
		}
	}
	// Last, so that a claim is resolved against everything this command has
	// already queued rather than against a half-built picture.
	for i := range pending.claims {
		if err := a.generate(ctx, tx, cs, &pending.claims[i], res); err != nil {
			return err
		}
	}
	return nil
}

// retireGenerated queues the removal of everything a record generated.
func (a *Applier) retireGenerated(
	ctx context.Context, tx store.Tx, cs *changeSet, source *zone.Record,
) error {
	// Only these three can have anything hanging off them today: an address
	// record generates a reverse entry, and a reverse entry inside a classless
	// child generates the delegation that points at it. Checking the type keeps
	// a bulk delete from asking about every record it touches.
	switch source.Type {
	case zone.TypeA, zone.TypeAAAA, zone.TypePTR:
	default:
		return nil
	}

	return a.retireBelow(ctx, tx, cs, source.ID)
}

// retire queues a generated record and everything hanging off it.
func (a *Applier) retire(
	ctx context.Context, tx store.Tx, cs *changeSet, rec *zone.Record,
) error {
	z, err := a.zoneOf(ctx, tx, cs, rec.ZoneID)
	if err != nil {
		return err
	}
	cs.in(z).remove(rec)
	return a.retireBelow(ctx, tx, cs, rec.ID)
}

// retireBelow queues everything generated from a record, however deep the chain
// runs: a delegation hangs off an entry, which hangs off an address record.
func (a *Applier) retireBelow(
	ctx context.Context, tx store.Tx, cs *changeSet, rid zone.RecordID,
) error {
	queue := []zone.RecordID{rid}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]

		derived, err := tx.ManagedBy(ctx, next)
		if err != nil {
			return err
		}
		for _, d := range derived {
			z, zerr := a.zoneOf(ctx, tx, cs, d.ZoneID)
			if zerr != nil {
				return zerr
			}
			cs.in(z).remove(d)
			queue = append(queue, d.ID)
		}
	}
	return nil
}

// generate queues the reverse entry an address record implies.
func (a *Applier) generate(
	ctx context.Context, tx store.Tx, cs *changeSet, source *zone.Record, res *Result,
) error {
	addr, isAddr := source.Address()
	if !isAddr || source.IsManaged() {
		return nil
	}
	if source.Disabled {
		// A record that answers nothing has nothing to reverse.
		return nil
	}

	rev, err := tx.ReverseZoneFor(ctx, addr)
	if errors.Is(err, store.ErrNotFound) {
		hint, herr := suggestReverseZone(addr)
		if herr != nil {
			return herr
		}
		hint.Source, hint.SourceName, hint.Address = source.ID, source.Name, addr
		res.MissingZones = append(res.MissingZones, hint)
		return nil
	}
	if err != nil {
		return err
	}

	owner, err := rev.ReverseOwner(addr)
	if err != nil {
		return err
	}
	key := zone.RRsetKey{Name: owner, Class: zone.ClassIN, Type: zone.TypePTR}

	existing, err := a.reverseRRset(ctx, tx, cs, rev, key)
	if err != nil {
		return err
	}

	// Already answering with this very name, so there is nothing to do. This is
	// what makes re-applying the same change a no-op.
	for i := range existing {
		if existing[i].RData.String() == source.Name.String() {
			return nil
		}
	}

	if len(existing) > 0 {
		return a.resolveConflict(ctx, tx, cs, rev, source, addr, key, existing, res)
	}
	ptr, err := a.queueGenerated(cs, rev, source, key)
	if err != nil {
		return err
	}
	return a.queueDelegation(ctx, tx, cs, rev, addr, key.Name, ptr, res)
}

// queueDelegation writes the parent-side CNAME that makes a classless reverse
// entry reachable (RFC 2317 §4).
//
// A resolver asked about 192.0.2.10 looks up "10.2.0.192.in-addr.arpa.", which
// lives in the /24, not in the classless child that actually answers for the
// address. The /24 therefore carries a CNAME pointing into the child. Doing
// that by hand for every address is the tedious, error-prone half of RFC 2317,
// and taking care of it is the point of the automation (D7).
func (a *Applier) queueDelegation(
	ctx context.Context, tx store.Tx, cs *changeSet, child *zone.Zone,
	addr netip.Addr, target zone.Name, ptr *zone.Record, res *Result,
) error {
	plain, err := zone.ReverseName(addr)
	if err != nil {
		return err
	}
	if plain.IsSubDomainOf(child.Name) {
		// An ordinary reverse zone answers under its own name, so there is
		// nothing to delegate.
		return nil
	}

	parentName, ok := child.Name.Parent()
	if !ok {
		return nil
	}
	parent, err := tx.ZoneByName(ctx, parentName)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}

	held, err := a.ownerAsWillBe(ctx, tx, cs, parent, plain)
	if err != nil {
		return err
	}
	for i := range held {
		if held[i].Type == zone.TypeCNAME && held[i].RData.String() == target.String() {
			return nil // already delegated
		}
	}
	if len(held) > 0 {
		// RFC 2181 §10.1: where a CNAME is, nothing else may be. Something is
		// already answering for this address in the parent, and taking it away
		// to install a delegation is not this automation's call.
		pol, perr := a.effectivePolicy(ctx, tx)
		if perr != nil {
			return perr
		}
		res.Conflicts = append(res.Conflicts, Conflict{
			Source:      ptr.ManagedBy,
			SourceName:  ptr.Name,
			Address:     addr,
			ReverseZone: parent.ID,
			ReverseName: plain,
			Existing:    held[0].Name,
			Generated:   held[0].IsManaged(),
			Policy:      pol,
		})
		return nil
	}

	data, err := zone.ParseRData(zone.TypeCNAME, zone.ClassIN, target.String())
	if err != nil {
		return fmt.Errorf("%w: %q is not usable as a delegation target: %w", zone.ErrInvalid, target, err)
	}
	cs.in(parent).insert(&zone.Record{
		ID:          zone.RecordID(id.New()),
		ZoneID:      parent.ID,
		Name:        plain,
		Class:       zone.ClassIN,
		Type:        zone.TypeCNAME,
		TTL:         parent.DefaultTTL,
		RData:       data,
		ManagedBy:   ptr.ID,
		ManagedKind: zone.ManagedRFC2317CNAME,
	})
	return nil
}

// ownerAsWillBe returns everything at one owner name as this change set will
// leave it.
func (a *Applier) ownerAsWillBe(
	ctx context.Context, tx store.Tx, cs *changeSet, z *zone.Zone, name zone.Name,
) ([]zone.Record, error) {
	stored, err := ownerRecords(ctx, tx, z.ID, name)
	if err != nil {
		return nil, err
	}
	pending := cs.pending(z.ID)

	out := stored[:0]
	for i := range stored {
		if !pending.removing(stored[i].ID) {
			out = append(out, stored[i])
		}
	}
	return append(out, pending.arrivingAt(name)...), nil
}

// resolveConflict applies the configured policy to an address that already
// answers a reverse lookup.
func (a *Applier) resolveConflict(
	ctx context.Context, tx store.Tx, cs *changeSet, rev *zone.Zone, source *zone.Record,
	addr netip.Addr, key zone.RRsetKey, existing []zone.Record, res *Result,
) error {
	pol := cs.policy
	if pol == "" {
		var perr error
		if pol, perr = a.effectivePolicy(ctx, tx); perr != nil {
			return perr
		}
	}

	held := &existing[0]
	conflict := Conflict{
		Source:      source.ID,
		SourceName:  source.Name,
		Address:     addr,
		ReverseZone: rev.ID,
		ReverseName: key.Name,
		Existing:    zone.Name{},
		Generated:   held.IsManaged(),
		Policy:      pol,
	}
	if n, err := zone.ParseName(held.RData.String()); err == nil {
		conflict.Existing = n
	}

	switch pol {
	case PolicyMulti:
		ptr, err := a.queueGenerated(cs, rev, source, key)
		if err != nil {
			return err
		}
		return a.queueDelegation(ctx, tx, cs, rev, addr, key.Name, ptr, res)

	case PolicyReject:
		return fmt.Errorf(
			"%w: %v already reverses to %q, and this server is set to refuse a second name for "+
				"one address; remove that entry, detach it, or change the reverse policy",
			store.ErrConflict, addr, conflict.Existing)

	case PolicyLastWins:
		// An entry someone wrote by hand is theirs. Detaching a generated entry
		// is how a person says "leave this one alone" (D4), and last-wins must
		// honour that or detaching would mean nothing.
		if !held.IsManaged() {
			res.Conflicts = append(res.Conflicts, conflict)
			return nil
		}
		for i := range existing {
			if existing[i].IsManaged() {
				cs.in(rev).remove(&existing[i])
			}
		}
		res.Conflicts = append(res.Conflicts, conflict)
		ptr, err := a.queueGenerated(cs, rev, source, key)
		if err != nil {
			return err
		}
		return a.queueDelegation(ctx, tx, cs, rev, addr, key.Name, ptr, res)

	default: // PolicyFirstWins
		res.Conflicts = append(res.Conflicts, conflict)
		return nil
	}
}

// queueGenerated adds one generated reverse entry to the change set.
func (a *Applier) queueGenerated(
	cs *changeSet, rev *zone.Zone, source *zone.Record, key zone.RRsetKey,
) (*zone.Record, error) {
	data, err := zone.ParseRData(zone.TypePTR, zone.ClassIN, source.Name.String())
	if err != nil {
		return nil, fmt.Errorf("%w: %q is not usable as reverse data: %w",
			zone.ErrInvalid, source.Name, err)
	}

	ptr := &zone.Record{
		ID:          zone.RecordID(id.New()),
		ZoneID:      rev.ID,
		Name:        key.Name,
		Class:       key.Class,
		Type:        key.Type,
		TTL:         rev.DefaultTTL,
		RData:       data,
		ManagedBy:   source.ID,
		ManagedKind: zone.ManagedPTR,
	}
	cs.in(rev).insert(ptr)
	return ptr, nil
}

// reverseRRset returns the reverse entries at a name as this change set will
// leave them: what is stored, less what is on its way out, plus what is on its
// way in. Two address records added in one command have to see each other.
func (a *Applier) reverseRRset(
	ctx context.Context, tx store.Tx, cs *changeSet, rev *zone.Zone, key zone.RRsetKey,
) ([]zone.Record, error) {
	stored, err := rrset(ctx, tx, rev.ID, key)
	if err != nil {
		return nil, err
	}

	pending := cs.pending(rev.ID)
	out := stored[:0]
	for i := range stored {
		if !pending.removing(stored[i].ID) {
			out = append(out, stored[i])
		}
	}
	return append(out, pending.arriving(key)...), nil
}

// zoneOf reads a zone, preferring one the change set already holds so that the
// same zone is not loaded twice and, more importantly, so that one serial is
// not read from two copies.
func (a *Applier) zoneOf(
	ctx context.Context, tx store.Tx, cs *changeSet, zid zone.ZoneID,
) (*zone.Zone, error) {
	if z, ok := cs.zones[zid]; ok {
		return z, nil
	}
	return tx.ZoneByID(ctx, zid)
}

// sameReverse reports whether two versions of a record imply the same reverse
// entry, so that an edit to a comment does not tear one down and build it again.
func sameReverse(before, after *zone.Record) bool {
	b, bok := before.Address()
	c, cok := after.Address()
	if bok != cok {
		return false
	}
	if !bok {
		return true
	}
	return b == c && before.Name.Equal(after.Name) && before.Disabled == after.Disabled
}

// suggestReverseZone names the zone that would answer for an address.
func suggestReverseZone(addr netip.Addr) (MissingZone, error) {
	bits := 24
	if addr.Unmap().Is6() {
		bits = 64
	}
	prefix, err := addr.Unmap().Prefix(bits)
	if err != nil {
		return MissingZone{}, fmt.Errorf("%w: %v has no /%d", zone.ErrInvalid, addr, bits)
	}
	name, err := zone.ReverseZoneName(prefix)
	if err != nil {
		return MissingZone{}, err
	}
	return MissingZone{Suggested: name, Prefix: prefix}, nil
}
