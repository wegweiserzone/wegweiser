package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// MemberID returns the identifier this node is a member of a cluster by, and
// the empty string when it has never been given one.
func (r reader) MemberID(ctx context.Context) (string, error) {
	var id string
	err := r.q.QueryRowContext(ctx, `SELECT member_id FROM node_identity WHERE id = 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// SetMemberID records the identifier this node is a member by, once.
func (t *txn) SetMemberID(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("%w: a member needs an identifier, and the empty string is none",
			zone.ErrInvalid)
	}
	held, err := t.MemberID(ctx)
	if err != nil {
		return err
	}
	switch {
	case held == id:
		return nil
	case held != "":
		return fmt.Errorf("%w: this node is member %q and cannot become %q", store.ErrConflict, held, id)
	}
	_, err = t.q.ExecContext(ctx,
		`INSERT INTO node_identity (id, member_id, created_at) VALUES (1, ?, ?)`,
		id, t.stamp().UnixMilli())
	return err
}
