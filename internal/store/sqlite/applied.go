package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
)

// AppliedIndex returns how far into the replicated log this node has got.
func (r reader) AppliedIndex(ctx context.Context) (uint64, error) {
	// SQLite counts in signed integers, so the column is checked on the way in
	// and on the way out rather than converted and hoped for.
	var index int64
	err := r.q.QueryRowContext(ctx,
		`SELECT log_index FROM applied_index WHERE id = 1`).Scan(&index)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if index <= 0 {
		return 0, fmt.Errorf("sqlite: the applied index reads %d, which is no position in a log", index)
	}
	return uint64(index), nil
}

// SetAppliedIndex records how far into the replicated log this node has got.
func (t *txn) SetAppliedIndex(ctx context.Context, index uint64) error {
	if index == 0 {
		return errors.New("sqlite: zero is no position in a log")
	}
	if index > math.MaxInt64 {
		return fmt.Errorf("sqlite: the log index %d is past what the column holds", index)
	}
	_, err := t.q.ExecContext(ctx, `
		INSERT INTO applied_index (id, log_index, updated_at) VALUES (1, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			log_index = excluded.log_index, updated_at = excluded.updated_at`,
		int64(index), t.stamp().UnixMilli())
	return err
}
