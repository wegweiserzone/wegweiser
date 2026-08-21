package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/wegweiserzone/wegweiser/internal/store"
)

// Setting returns one JSON-encoded setting value.
func (r reader) Setting(ctx context.Context, key string) ([]byte, error) {
	var value string
	err := r.q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("setting named", key)
	}
	if err != nil {
		return nil, err
	}
	return []byte(value), nil
}

// PutSetting stores a JSON-encoded setting value, replacing any previous one.
func (t *txn) PutSetting(ctx context.Context, key string, value []byte) error {
	if key == "" {
		return errors.New("sqlite: a setting needs a key")
	}
	if !json.Valid(value) {
		// The column holds JSON by contract, and every reader unmarshals it.
		// Catching it here means the bad value never becomes something a later
		// read has to fail on.
		return fmt.Errorf("sqlite: the value for setting %q is not JSON", key)
	}

	_, err := t.q.ExecContext(ctx, `
		INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, string(value), t.stamp().UnixMilli())
	return err
}

// Compile-time proof that this package satisfies the boundary it implements.
// Without it, a signature drifting out of line would only be noticed by whoever
// next tried to wire the store up.
var (
	_ store.Store  = (*Store)(nil)
	_ store.Reader = (*reader)(nil)
	_ store.Tx     = (*txn)(nil)
)
