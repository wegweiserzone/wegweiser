package journal

import (
	"fmt"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/id"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// CommitID identifies a commit. Like the other identifiers it is a ULID, so
// commits sort by creation time without a second index.
type CommitID string

// Kind classifies what a commit did. It exists for the audit log and the UI,
// which want to say "imported" or "rolled back" rather than reciting the
// events.
type Kind string

const (
	// KindZoneCreate is the first commit of a zone, which brings the zone into
	// existence along with its initial records.
	KindZoneCreate Kind = "zone_create"
	// KindZoneUpdate changed the zone's own settings rather than its records.
	KindZoneUpdate Kind = "zone_update"
	// KindZoneDelete removed the zone.
	KindZoneDelete Kind = "zone_delete"
	// KindEdit is an ordinary record change.
	KindEdit Kind = "edit"
	// KindImport applied a zonefile or a declarative configuration.
	KindImport Kind = "import"
	// KindRollback restored an earlier state of the zone by moving forward to
	// it; see data model §4.6.
	KindRollback Kind = "rollback"
)

// Valid reports whether k is one of the defined kinds.
func (k Kind) Valid() bool {
	switch k {
	case KindZoneCreate, KindZoneUpdate, KindZoneDelete, KindEdit, KindImport, KindRollback:
		return true
	default:
		return false
	}
}

// Source names the interface a commit arrived through, so the audit log can
// distinguish a change someone made in the UI from one a config import made.
type Source string

const (
	// SourceAPI is a change made over the HTTP API, which includes the GUI.
	SourceAPI Source = "api"
	// SourceCLI is a change made with the weg command.
	SourceCLI Source = "cli"
	// SourceImport is a change made by a zonefile or configuration import.
	SourceImport Source = "import"
	// SourceSystem is a change the server made on its own, such as the reverse
	// automation reconciling a zone.
	SourceSystem Source = "system"
)

// Valid reports whether s is one of the defined sources.
func (s Source) Valid() bool {
	switch s {
	case SourceAPI, SourceCLI, SourceImport, SourceSystem:
		return true
	default:
		return false
	}
}

// Commit is one atomic change to one zone.
type Commit struct {
	ID     CommitID
	ZoneID zone.ZoneID
	// ZoneName is carried on the commit rather than joined from the zone,
	// because a commit outlives the zone: the last thing that happens to a zone
	// is that someone deletes it, and there would be nothing left to join to.
	ZoneName zone.Name

	// SerialFrom is the zone serial this commit was applied to, and SerialTo
	// the serial it produced.
	SerialFrom zone.Serial
	SerialTo   zone.Serial

	Kind   Kind
	Source Source
	// Actor names who caused the change: an API token's name, a shell user, or
	// empty for the system itself.
	Actor   string
	Comment string

	// RevertsTo is the serial a rollback restored the zone to. It is nil for
	// every other kind: a pointer rather than a zero value, because serial 0
	// is a legal serial and could not be told apart from "not a rollback".
	RevertsTo *zone.Serial

	// Events are the record changes, ordered by Seq with every deletion before
	// every addition. Empty is legal: a commit that changed only the zone's own
	// settings moved no records.
	Events []Event

	CreatedAt time.Time
}

// Validate reports whether the commit is well formed on its own. It cannot
// check that the events belong to the zone, or that they apply to the state at
// SerialFrom; both need more than the commit.
func (c Commit) Validate() error {
	if !id.Valid(string(c.ID)) {
		return fmt.Errorf("%w: a commit needs an identifier, and %q is not one", zone.ErrInvalid, c.ID)
	}
	if !id.Valid(string(c.ZoneID)) {
		return fmt.Errorf("%w: a commit belongs to a zone, and %q is not a zone identifier",
			zone.ErrInvalid, c.ZoneID)
	}
	if c.ZoneName.IsZero() {
		return fmt.Errorf("%w: a commit records which zone it changed by name as well as by "+
			"identifier, so that it still reads once the zone is gone", zone.ErrInvalid)
	}
	if !c.Kind.Valid() {
		return fmt.Errorf("%w commit kind %q", zone.ErrInvalid, c.Kind)
	}
	if !c.Source.Valid() {
		return fmt.Errorf("%w commit source %q", zone.ErrInvalid, c.Source)
	}
	if err := c.validateSerials(); err != nil {
		return err
	}
	return c.validateEvents()
}

// validateSerials enforces the one-commit-one-step rule that rollback and
// incremental transfer both rest on.
func (c Commit) validateSerials() error {
	if c.Kind == KindZoneCreate {
		// A zone that did not exist has no serial to step from. Its first
		// commit sets the starting serial instead, which an import needs: a
		// zone migrated from another server keeps the serial its secondaries
		// have already seen, and that is rarely 1.
		if !c.SerialFrom.IsZero() {
			return fmt.Errorf("%w: a zone is created from serial 0, not from %s",
				zone.ErrInvalid, c.SerialFrom)
		}
		if c.SerialTo.IsZero() {
			return fmt.Errorf("%w: a zone must start at a non-zero serial (RFC 1912 §2.2)", zone.ErrInvalid)
		}
	} else if c.SerialTo != c.SerialFrom.Next() {
		return fmt.Errorf(
			"%w: a commit advances the serial by exactly one step, but this one goes from %s to %s; "+
				"without that, restoring a zone to a serial would not name a single state",
			zone.ErrInvalid, c.SerialFrom, c.SerialTo)
	}

	if (c.Kind == KindRollback) != (c.RevertsTo != nil) {
		if c.Kind == KindRollback {
			return fmt.Errorf("%w: a rollback must name the serial it restores", zone.ErrInvalid)
		}
		return fmt.Errorf("%w: only a rollback names a serial it restores, and this is a %s",
			zone.ErrInvalid, c.Kind)
	}
	if c.RevertsTo != nil {
		// Rolling back moves forward to an earlier state, so the target has to
		// lie behind the state being left. Serials wrap, so "behind" is only
		// meaningful where RFC 1982 §3.2 defines it.
		if !c.RevertsTo.Comparable(c.SerialFrom) {
			return fmt.Errorf("%w: serial %s is exactly half the serial space from %s, "+
				"so which one is earlier is undefined (RFC 1982 §3.2)",
				zone.ErrInvalid, c.RevertsTo, c.SerialFrom)
		}
		if !c.RevertsTo.Before(c.SerialFrom) {
			return fmt.Errorf("%w: a rollback restores an earlier state, but serial %s is not before %s",
				zone.ErrInvalid, c.RevertsTo, c.SerialFrom)
		}
	}
	return nil
}

// validateEvents enforces the ordering a consumer is allowed to rely on.
func (c Commit) validateEvents() error {
	sawAdd := false
	for i := range c.Events {
		e := &c.Events[i]
		if err := e.Validate(); err != nil {
			return err
		}
		if e.Seq != i {
			return fmt.Errorf("%w: event %d carries sequence number %d; events are numbered from zero without gaps",
				zone.ErrInvalid, i, e.Seq)
		}
		switch e.Op {
		case OpAdd:
			sawAdd = true
		case OpDel:
			// RFC 1995 §2 frames a difference sequence as the deletions
			// followed by the additions. Keeping the events in that order means
			// a commit is one such sequence, with nothing to sort at transfer
			// time.
			if sawAdd {
				return fmt.Errorf(
					"%w: event %d deletes a record after an addition; every deletion in a commit comes "+
						"first, so that the commit is a difference sequence as it stands (RFC 1995 §2)",
					zone.ErrInvalid, i)
			}
		}
	}
	return nil
}
