package apply

import (
	"context"
	"errors"
	"fmt"

	"github.com/wegweiserzone/wegweiser/internal/journal"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// ZoneOpKind is what a batch does to a zone itself, as opposed to what it
// holds.
type ZoneOpKind string

const (
	// ZoneCreate brings a zone into being.
	ZoneCreate ZoneOpKind = "create"
	// ZoneUpdate replaces a zone's own settings.
	ZoneUpdate ZoneOpKind = "update"
	// ZoneDelete removes a zone, and its records with it.
	ZoneDelete ZoneOpKind = "delete"
)

// ZoneOp is one change to a zone itself.
type ZoneOp struct {
	Kind   ZoneOpKind
	ZoneID zone.ZoneID

	// Zone is what the zone should become. Nil for a deletion.
	Zone *zone.Zone
}

// Batch is one command resolved against the zones as they stand, ready to be
// carried out without deciding anything further. A cluster replicates this
// rather than the command; see docs/decisions/d24-what-the-cluster-replicates.md.
type Batch struct {
	set *changeSet

	// Commits explain the work, one per zone the change reached.
	Commits []*journal.Commit

	// Zones are the changes to the zones themselves. They bracket the record
	// changes: a record needs its zone to exist, and a zone outlives the
	// journal entry that removes it.
	Zones []ZoneOp

	// Settings are the server settings this change writes. They are not zone
	// data and produce no commit, so a batch can carry these and nothing else
	// (D32).
	Settings []SettingChange

	// Keys are the changes to the keys a secondary signs with, for the same
	// reason and with the same consequence.
	Keys []KeyOp

	// Tokens are the changes to the tokens this server accepts, likewise.
	Tokens []TokenOp
}

// Applied says what a batch changed, for anything that keeps a copy of what the
// store holds somewhere else.
//
// It names what was touched rather than carrying it, so the copy is rebuilt
// from the store. Two of these handled out of order then still end at the
// latest state, because by the time either is handled the store holds it.
type Applied struct {
	// Zones are the zones the batch changed, each once. A deleted zone is among
	// them: the store no longer holds it, and that is how a reader learns to
	// drop it.
	Zones []AppliedZone

	// Keys is whether the transfer keys changed.
	Keys bool

	// Settings are the keys of the server settings the batch wrote.
	Settings []string
}

// AppliedZone names one zone a batch changed. The name travels beside the
// identifier because a zone that was deleted can no longer be looked up by it.
type AppliedZone struct {
	ID   zone.ZoneID
	Name zone.Name
}

// touched reports what the batch changed.
//
// The zones are read off the commits. Everything that changes a zone commits,
// which is what [Batch.Empty] leans on as well.
func (b *Batch) touched() Applied {
	var out Applied
	seen := make(map[zone.ZoneID]struct{}, len(b.Commits))
	for _, c := range b.Commits {
		if _, dup := seen[c.ZoneID]; dup {
			continue
		}
		seen[c.ZoneID] = struct{}{}
		out.Zones = append(out.Zones, AppliedZone{ID: c.ZoneID, Name: c.ZoneName})
	}
	out.Keys = len(b.Keys) > 0
	for _, s := range b.Settings {
		out.Settings = append(out.Settings, s.Key)
	}
	return out
}

// zoneOp returns the batch's change to a zone itself, or nil.
func (b *Batch) zoneOp(zid zone.ZoneID) *ZoneOp {
	for i := range b.Zones {
		if b.Zones[i].ZoneID == zid {
			return &b.Zones[i]
		}
	}
	return nil
}

// Empty reports whether the command changed nothing.
//
// A commit is not the test on its own. Everything that touches a zone produces
// one, and a setting touches no zone.
func (b *Batch) Empty() bool {
	return b == nil ||
		(len(b.Commits) == 0 && len(b.Settings) == 0 &&
			len(b.Keys) == 0 && len(b.Tokens) == 0)
}

// applied reports whether the journal already holds this batch.
//
// The commit identifiers are minted once, by whoever planned the change, so
// they name the same change on every node and the journal is the record of
// what has been carried out.
func (b *Batch) applied(ctx context.Context, r store.Reader) (bool, error) {
	seen := 0
	for _, c := range b.Commits {
		_, err := r.CommitByID(ctx, c.ID)
		if err == nil {
			seen++
			continue
		}
		if !errors.Is(err, store.ErrNotFound) {
			return false, err
		}
	}
	if seen > 0 && seen < len(b.Commits) {
		return false, fmt.Errorf(
			"%w: %d of this batch's %d commits are in the journal; a batch lands whole or not at all",
			store.ErrConflict, seen, len(b.Commits))
	}
	return seen > 0, nil
}

// planned carries the batch out inside a planning transaction and checks what
// it produced. Anything a batch can be refused for is refused here.
func (b *Batch) planned(ctx context.Context, tx store.Tx) error {
	if err := b.write(ctx, tx); err != nil {
		return err
	}
	return b.validate(ctx, tx)
}

// validate checks the owner names the batch touched against the state the
// writes produced, rather than against a model of it: a model is a second
// version of the same thing, and the two drift.
func (b *Batch) validate(ctx context.Context, tx store.Tx) error {
	for _, commit := range b.Commits {
		ch := b.set.byZone[commit.ZoneID]
		if ch == nil {
			continue
		}
		z, err := tx.ZoneByID(ctx, commit.ZoneID)
		if err != nil {
			return err
		}
		if err := validateTouched(ctx, tx, z, ch.touched); err != nil {
			return err
		}
	}
	return nil
}

// write carries the batch out. It decides nothing and refuses nothing: a
// follower has to be unable to disagree with the node that planned the change,
// so everything that could be objected to was objected to by [Batch.planned].
func (b *Batch) write(ctx context.Context, tx store.Tx) error {
	// First, so that anything below reading a setting reads the one this batch
	// carries. Nothing does today, and the day something does it should not
	// depend on where in this function it sits.
	for _, c := range b.Settings {
		if err := tx.PutSetting(ctx, c.Key, c.Value); err != nil {
			return err
		}
	}
	if err := b.writeKeys(ctx, tx); err != nil {
		return err
	}
	if err := b.writeTokens(ctx, tx); err != nil {
		return err
	}

	for _, op := range b.Zones {
		switch op.Kind {
		case ZoneCreate:
			if err := tx.CreateZone(ctx, op.Zone); err != nil {
				return err
			}
		case ZoneUpdate:
			if err := tx.UpdateZone(ctx, op.Zone); err != nil {
				return err
			}
		case ZoneDelete:
			// After the commits, below.
		default:
			return fmt.Errorf("%w zone operation %q", zone.ErrInvalid, op.Kind)
		}
	}

	// A batch that carries only settings has no resolved zone changes at all,
	// so there is nothing here to write and no set to write it from.
	if b.set != nil {
		if err := b.set.write(ctx, tx); err != nil {
			return err
		}
	}

	for _, commit := range b.Commits {
		ch := b.set.byZone[commit.ZoneID]
		op := b.zoneOp(commit.ZoneID)
		if ch == nil && op == nil {
			return fmt.Errorf("%w: the batch commits to zone %s without changing it",
				zone.ErrInvalid, commit.ZoneID)
		}

		// Where the batch changes the zone itself, that operation has already
		// put the serial where it belongs.
		if ch != nil && op == nil {
			// Read, not carried: the zone is replicated state, so a copy
			// travelling in the batch could only disagree with it.
			z, err := tx.ZoneByID(ctx, commit.ZoneID)
			if err != nil {
				return err
			}
			if err := writeSOA(ctx, tx, z, ch, commit); err != nil {
				return err
			}
		}
		if err := tx.AppendCommit(ctx, commit); err != nil {
			return err
		}
	}

	for _, op := range b.Zones {
		if op.Kind != ZoneDelete {
			continue
		}
		if err := tx.DeleteZone(ctx, op.ZoneID); err != nil {
			return err
		}
	}
	return nil
}
