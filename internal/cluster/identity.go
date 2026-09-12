package cluster

import (
	"context"
	"fmt"

	"github.com/wegweiserzone/wegweiser/internal/id"
	"github.com/wegweiserzone/wegweiser/internal/store"
)

// Identity returns the identifier this node is a member by, minting one the
// first time it is asked (docs/decisions/d42-membership-lives-in-the-log.md).
//
// configured is what the configuration file names, and may be empty. The
// first time, a named identifier is taken as it stands; after that the file is
// held to it. A member whose identifier changed would graft one member's
// history onto another's, so a file that disagrees with what the store holds
// is refused rather than obeyed.
func Identity(ctx context.Context, st store.Store, configured string) (string, error) {
	var member string
	err := st.Update(ctx, func(tx store.Tx) error {
		held, err := tx.MemberID(ctx)
		if err != nil {
			return err
		}
		switch {
		case held != "" && configured != "" && held != configured:
			return fmt.Errorf("cluster: this node is member %q and its configuration names it %q; "+
				"a member's identifier never changes (docs/decisions/d42-membership-lives-in-the-log.md)",
				held, configured)
		case held != "":
			member = held
			return nil
		case configured != "":
			member = configured
		default:
			member = id.New()
		}
		return tx.SetMemberID(ctx, member)
	})
	if err != nil {
		return "", err
	}
	return member, nil
}
