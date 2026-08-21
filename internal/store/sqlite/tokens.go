package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/id"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// The name trips gosec's hardcoded-credential rule, which matches on the
// identifier rather than on the value; this is a list of column names.
//
//nolint:gosec // G101: a SQL projection, not a secret
const tokenColumns = `
	id, name, prefix, hash, scopes, created_at, last_used_at, expires_at, revoked_at`

// TokenByHash resolves an API token by the SHA-256 of its secret.
//
// The filtering happens in the query rather than after it, so that unknown,
// revoked and expired all leave by the same door: a caller comparing how long
// the three take, or which error comes back, learns nothing about which tokens
// exist.
func (r reader) TokenByHash(ctx context.Context, hash []byte) (*store.Token, error) {
	if len(hash) == 0 {
		return nil, notFound("token with the hash", "(empty)")
	}
	now := r.now().UTC().UnixMilli()

	row := r.q.QueryRowContext(ctx,
		`SELECT`+tokenColumns+` FROM api_tokens
		 WHERE hash = ? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)`,
		hash, now)

	t, err := scanToken(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: no usable token", store.ErrNotFound)
	}
	return t, err
}

// ListTokens returns every token, revoked and expired ones included, so the UI
// can show a full history. Secrets are not stored and cannot be returned.
func (r reader) ListTokens(ctx context.Context) (_ []*store.Token, err error) {
	rows, err := r.q.QueryContext(ctx, `SELECT`+tokenColumns+` FROM api_tokens ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()

	var out []*store.Token
	for rows.Next() {
		t, serr := scanToken(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CreateToken stores a new API token.
func (t *txn) CreateToken(ctx context.Context, tok *store.Token) error {
	if tok == nil {
		return errors.New("sqlite: no token given")
	}
	if !id.Valid(string(tok.ID)) {
		return fmt.Errorf("%w: a token needs an identifier assigned before it is stored, and %q is not one",
			zone.ErrInvalid, tok.ID)
	}
	if len(tok.Hash) != sha256Size {
		return fmt.Errorf("%w: a token hash is %d bytes of SHA-256, not %d",
			zone.ErrInvalid, sha256Size, len(tok.Hash))
	}

	if tok.CreatedAt.IsZero() {
		tok.CreatedAt = t.stamp()
	} else {
		tok.CreatedAt = fromMillis(tok.CreatedAt.UnixMilli())
	}

	scopes, err := json.Marshal(scopesOrEmpty(tok.Scopes))
	if err != nil {
		return fmt.Errorf("sqlite: encoding the token scopes: %w", err)
	}

	_, err = t.q.ExecContext(ctx, `
		INSERT INTO api_tokens (id, name, prefix, hash, scopes, created_at, last_used_at, expires_at, revoked_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		string(tok.ID), tok.Name, tok.Prefix, tok.Hash, string(scopes),
		tok.CreatedAt.UnixMilli(), nullMillis(tok.LastUsedAt),
		nullMillis(tok.ExpiresAt), nullMillis(tok.RevokedAt))

	return translate(err, "a token with that secret already exists")
}

// RevokeToken marks a token unusable. Tokens are never deleted, so that the
// audit log keeps naming the token behind each change.
func (t *txn) RevokeToken(ctx context.Context, tid store.TokenID, at time.Time) error {
	// Only the first revocation counts: re-revoking must not move the moment
	// the token stopped working.
	res, err := t.q.ExecContext(ctx,
		`UPDATE api_tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
		at.UTC().UnixMilli(), string(tid))
	if err != nil {
		return err
	}

	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		// Either it does not exist or it was revoked already. Distinguish, so
		// that revoking twice is not reported as a missing token.
		var revoked sql.NullInt64
		err := t.q.QueryRowContext(ctx, `SELECT revoked_at FROM api_tokens WHERE id = ?`,
			string(tid)).Scan(&revoked)
		if errors.Is(err, sql.ErrNoRows) {
			return notFound("token with the identifier", tid)
		}
		return err
	}
	return nil
}

// TouchToken records that a token authenticated a request.
func (t *txn) TouchToken(ctx context.Context, tid store.TokenID, at time.Time) error {
	// Advisory, so a token that has just been deleted is not an error worth
	// failing a request that already succeeded over.
	_, err := t.q.ExecContext(ctx, `UPDATE api_tokens SET last_used_at = ? WHERE id = ?`,
		at.UTC().UnixMilli(), string(tid))
	return err
}

// sha256Size is the length of the stored token hash, mirrored by a CHECK in the
// schema.
const sha256Size = 32

func scanToken(row scannable) (*store.Token, error) {
	var (
		t                          store.Token
		tid                        string
		scopes                     string
		created                    int64
		lastUsed, expires, revoked sql.NullInt64
	)

	if err := row.Scan(&tid, &t.Name, &t.Prefix, &t.Hash, &scopes,
		&created, &lastUsed, &expires, &revoked); err != nil {
		return nil, err
	}

	t.ID = store.TokenID(tid)
	if err := json.Unmarshal([]byte(scopes), &t.Scopes); err != nil {
		return nil, corrupt("api_tokens", tid, "scopes", err)
	}
	t.CreatedAt = fromMillis(created)
	t.LastUsedAt = fromNullMillis(lastUsed)
	t.ExpiresAt = fromNullMillis(expires)
	t.RevokedAt = fromNullMillis(revoked)

	return &t, nil
}

// scopesOrEmpty keeps a nil slice out of the column, so the stored value is
// always a JSON array and never the word null.
func scopesOrEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nullMillis(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().UnixMilli()
}

func fromNullMillis(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return fromMillis(v.Int64)
}
