package apply

import (
	"fmt"

	"github.com/wegweiserzone/wegweiser/internal/journal"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// OpAction is what one step of a [Command] asks for.
type OpAction string

const (
	// ActionAdd adds one record.
	ActionAdd OpAction = "add"
	// ActionUpdate replaces one record's content, keeping its identity so that
	// its comment, its provenance and the diff line pointing at it survive.
	ActionUpdate OpAction = "update"
	// ActionDelete removes one record.
	ActionDelete OpAction = "delete"
	// ActionReplaceRRset makes an RRset exactly the given set of records. An
	// empty set removes it. This is what a record editor submits, because it
	// is the unit DNS answers with (RFC 2181 §5).
	ActionReplaceRRset OpAction = "replace_rrset"
	// ActionDetach turns a generated record into an authored one. It keeps its
	// data and its identity, loses its link to the record it came from, and the
	// automation stops touching it. See docs/decisions/ D4.
	ActionDetach OpAction = "detach"
)

// Valid reports whether a is one of the defined actions.
func (a OpAction) Valid() bool {
	switch a {
	case ActionAdd, ActionUpdate, ActionDelete, ActionReplaceRRset, ActionDetach:
		return true
	default:
		return false
	}
}

// RecordOp is one authored change inside a [Command].
type RecordOp struct {
	Action OpAction

	// RecordID addresses the record for [ActionUpdate] and [ActionDelete].
	RecordID zone.RecordID

	// Record is the content for [ActionAdd] and [ActionUpdate].
	Record *zone.Record

	// Key names the RRset for [ActionReplaceRRset]. It is given separately from
	// Records because removing an RRset entirely leaves no record to read it
	// from.
	Key zone.RRsetKey

	// Records is what the RRset should hold after [ActionReplaceRRset]. Empty
	// removes the set.
	Records []zone.Record
}

// Validate reports whether the operation is well formed on its own.
func (o RecordOp) Validate() error {
	if !o.Action.Valid() {
		return fmt.Errorf("%w operation %q", zone.ErrInvalid, o.Action)
	}

	switch o.Action {
	case ActionAdd:
		if o.Record == nil {
			return fmt.Errorf("%w: adding a record needs the record", zone.ErrInvalid)
		}
		return o.Record.Validate()

	case ActionUpdate:
		if o.RecordID == "" {
			return fmt.Errorf("%w: updating a record needs to say which one", zone.ErrInvalid)
		}
		if o.Record == nil {
			return fmt.Errorf("%w: updating a record needs the new content", zone.ErrInvalid)
		}
		if o.Record.ID != "" && o.Record.ID != o.RecordID {
			return fmt.Errorf("%w: the update names record %q but carries record %q",
				zone.ErrInvalid, o.RecordID, o.Record.ID)
		}
		return o.Record.Validate()

	case ActionDelete:
		if o.RecordID == "" {
			return fmt.Errorf("%w: deleting a record needs to say which one", zone.ErrInvalid)
		}
		return nil

	case ActionDetach:
		if o.RecordID == "" {
			return fmt.Errorf("%w: detaching a record needs to say which one", zone.ErrInvalid)
		}
		return nil

	case ActionReplaceRRset:
		if o.Key.Name.IsZero() {
			return fmt.Errorf("%w: replacing an RRset needs the name it sits at", zone.ErrInvalid)
		}
		for i := range o.Records {
			r := &o.Records[i]
			if r.Key() != o.Key {
				return fmt.Errorf(
					"%w: the replacement for the %s %s RRset at %q contains a %s record at %q; "+
						"an RRset holds one name, class and type",
					zone.ErrInvalid, o.Key.Class, o.Key.Type, o.Key.Name, r.Type, r.Name)
			}
		}
		// The members have to form a usable set among themselves before
		// anything is read from the database.
		return zone.ValidateRRset(o.Records)
	}
	return nil
}

// Command is the intent a client submits, before validation against the zone
// and before reverse automation expands it.
//
// It is the unit Raft will replicate; see
// docs/decisions/d19-journal-as-command-log.md. That is why the applier fills in
// everything undetermined, identifiers above all, before it starts, rather
// than while applying: a command has to produce the same result on every node
// that applies it.
type Command struct {
	ZoneID zone.ZoneID

	// Ops are the authored operations, applied in order.
	Ops []RecordOp

	// ExpectedSerial enables optimistic concurrency: the command is refused if
	// the zone has moved on since it was read. Nil skips the check. It is a
	// pointer because serial 0 is a legal serial and could not otherwise be
	// told apart from "do not check".
	ExpectedSerial *zone.Serial

	Kind    journal.Kind
	Source  journal.Source
	Actor   string
	Comment string
}

// Validate reports whether the command is well formed on its own. It says
// nothing about whether it applies to the zone as it currently stands.
func (c Command) Validate() error {
	if c.ZoneID == "" {
		return fmt.Errorf("%w: a command names the zone it changes", zone.ErrInvalid)
	}
	if len(c.Ops) == 0 {
		return fmt.Errorf("%w: a command with no operations changes nothing", zone.ErrInvalid)
	}
	if !c.Source.Valid() {
		return fmt.Errorf("%w command source %q", zone.ErrInvalid, c.Source)
	}
	// A command changes records, so the kinds that describe a zone's own
	// lifecycle do not belong to it.
	switch c.Kind {
	case journal.KindEdit, journal.KindImport, journal.KindRollback:
	default:
		return fmt.Errorf(
			"%w: %q does not describe a record change; a command is an edit, an import or a rollback",
			zone.ErrInvalid, c.Kind)
	}

	for i := range c.Ops {
		if err := c.Ops[i].Validate(); err != nil {
			return fmt.Errorf("operation %d: %w", i, err)
		}
	}
	return nil
}
