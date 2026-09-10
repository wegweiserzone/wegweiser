package store

import (
	"fmt"

	"github.com/wegweiserzone/wegweiser/internal/journal"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// ReplicatedKind says which field of a [Replicated] carries the item.
type ReplicatedKind string

const (
	// ReplicatedSetting is one server setting.
	ReplicatedSetting ReplicatedKind = "setting"
	// ReplicatedToken is one API token, without its secret, which this server
	// never held.
	ReplicatedToken ReplicatedKind = "token"
	// ReplicatedTSIGKey is one transfer key, with the secret it signs by.
	ReplicatedTSIGKey ReplicatedKind = "tsig_key"
	// ReplicatedZone is one zone, without the records in it.
	ReplicatedZone ReplicatedKind = "zone"
	// ReplicatedRecord is one record.
	ReplicatedRecord ReplicatedKind = "record"
	// ReplicatedCommit is one journal commit, with its events.
	ReplicatedCommit ReplicatedKind = "commit"
)

// Setting is one server setting together with the key it is stored under.
// Reading one by name answers with the value alone, a caller who names a key
// already holding the key; streaming them all has to carry both.
type Setting struct {
	Key string
	// Value is JSON, which is what the column holds and what every reader of a
	// setting unmarshals.
	Value []byte
}

// Replicated is one item of the content a cluster replicates. Exactly one of
// the fields below is set, and Kind says which.
//
// The list is closed, and closing it is the point:
// docs/decisions/d30-what-a-log-snapshot-contains.md says anything left out of
// a log snapshot is data a joining node silently does not have, and
// docs/decisions/d32-what-else-the-cluster-replicates.md widened the list to
// the tokens, keys and settings that reach other nodes in a batch.
//
// What is deliberately absent is node-local: how far this node has got through
// the log, the browser sessions it is holding, and when a token was last used
// *here*. That last one is the only node-local field living on a replicated
// row, and it is zero on a token that travels.
type Replicated struct {
	Kind ReplicatedKind

	Setting *Setting
	Token   *Token
	TSIGKey *TSIGKey
	Zone    *zone.Zone
	Record  *zone.Record
	Commit  *journal.Commit
}

// Validate reports whether the item carries exactly what its kind promises. It
// says nothing about whether the item itself is well formed, which is the
// business of whoever stores it.
func (r *Replicated) Validate() error {
	if r == nil {
		return fmt.Errorf("%w: no replicated item given", zone.ErrInvalid)
	}

	var carried []ReplicatedKind
	for _, f := range []struct {
		kind ReplicatedKind
		set  bool
	}{
		{ReplicatedSetting, r.Setting != nil},
		{ReplicatedToken, r.Token != nil},
		{ReplicatedTSIGKey, r.TSIGKey != nil},
		{ReplicatedZone, r.Zone != nil},
		{ReplicatedRecord, r.Record != nil},
		{ReplicatedCommit, r.Commit != nil},
	} {
		if f.set {
			carried = append(carried, f.kind)
		}
	}

	switch {
	case len(carried) == 0:
		return fmt.Errorf("%w: a replicated item of kind %q carries nothing", zone.ErrInvalid, r.Kind)
	case len(carried) > 1:
		return fmt.Errorf("%w: a replicated item carries %v at once, and it carries one",
			zone.ErrInvalid, carried)
	case carried[0] != r.Kind:
		return fmt.Errorf("%w: a replicated item of kind %q carries a %s instead",
			zone.ErrInvalid, r.Kind, carried[0])
	}
	return nil
}
