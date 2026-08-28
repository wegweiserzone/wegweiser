package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/id"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

const tsigColumns = `
	id, name, algorithm, secret, created_at, revoked_at`

// TSIGKeyByName resolves the key a transfer request names.
//
// A revoked key is not found. Its secret is gone, so nothing could be verified
// with it, and reporting it as present would only mean a second error later.
func (r reader) TSIGKeyByName(ctx context.Context, name zone.Name) (*store.TSIGKey, error) {
	if name.IsZero() {
		return nil, notFound("TSIG key named", "(empty)")
	}
	row := r.q.QueryRowContext(ctx,
		`SELECT`+tsigColumns+` FROM tsig_keys WHERE name = ? AND revoked_at IS NULL`,
		name.String())

	k, err := scanTSIGKey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("TSIG key named", name)
	}
	return k, err
}

// TSIGKeyByID returns one key, revoked or not.
func (r reader) TSIGKeyByID(ctx context.Context, kid store.TSIGKeyID) (*store.TSIGKey, error) {
	row := r.q.QueryRowContext(ctx, `SELECT`+tsigColumns+` FROM tsig_keys WHERE id = ?`, string(kid))

	k, err := scanTSIGKey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("TSIG key with the identifier", kid)
	}
	return k, err
}

// ListTSIGKeys returns every key, revoked ones included.
func (r reader) ListTSIGKeys(ctx context.Context) (_ []*store.TSIGKey, err error) {
	rows, err := r.q.QueryContext(ctx,
		`SELECT`+tsigColumns+` FROM tsig_keys ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()

	var out []*store.TSIGKey
	for rows.Next() {
		k, serr := scanTSIGKey(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// CreateTSIGKey stores a new key.
func (t *txn) CreateTSIGKey(ctx context.Context, k *store.TSIGKey) error {
	if k == nil {
		return errors.New("sqlite: no TSIG key given")
	}
	if !id.Valid(string(k.ID)) {
		return fmt.Errorf(
			"%w: a TSIG key needs an identifier assigned before it is stored, and %q is not one",
			zone.ErrInvalid, k.ID)
	}
	if k.Name.IsZero() {
		return fmt.Errorf("%w: a TSIG key needs a name both ends agree on", zone.ErrInvalid)
	}
	if !k.Algorithm.Valid() {
		return fmt.Errorf("%w: %q is not an algorithm this server signs with",
			zone.ErrInvalid, k.Algorithm)
	}
	if len(k.Secret) == 0 {
		return fmt.Errorf("%w: a TSIG key with no secret signs nothing", zone.ErrInvalid)
	}

	if k.CreatedAt.IsZero() {
		k.CreatedAt = t.stamp()
	} else {
		k.CreatedAt = fromMillis(k.CreatedAt.UnixMilli())
	}

	_, err := t.q.ExecContext(ctx, `
		INSERT INTO tsig_keys (id, name, algorithm, secret, created_at, revoked_at)
		VALUES (?,?,?,?,?,NULL)`,
		string(k.ID), k.Name.String(), string(k.Algorithm), k.Secret, k.CreatedAt.UnixMilli())

	return translate(err, "a key with that name already signs on this server")
}

// RevokeTSIGKey marks a key unusable and clears its secret.
func (t *txn) RevokeTSIGKey(ctx context.Context, kid store.TSIGKeyID, at time.Time) error {
	// Only the first revocation counts, and it takes the secret with it.
	res, err := t.q.ExecContext(ctx,
		`UPDATE tsig_keys SET revoked_at = ?, secret = NULL WHERE id = ? AND revoked_at IS NULL`,
		at.UTC().UnixMilli(), string(kid))
	if err != nil {
		return err
	}

	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		// Either it does not exist or it was revoked already, and revoking
		// twice is not a missing key.
		var revoked sql.NullInt64
		qerr := t.q.QueryRowContext(ctx, `SELECT revoked_at FROM tsig_keys WHERE id = ?`,
			string(kid)).Scan(&revoked)
		if errors.Is(qerr, sql.ErrNoRows) {
			return notFound("TSIG key with the identifier", kid)
		}
		return qerr
	}
	return nil
}

func scanTSIGKey(row scannable) (*store.TSIGKey, error) {
	var (
		k         store.TSIGKey
		kid, name string
		algorithm string
		secret    []byte
		created   int64
		revoked   sql.NullInt64
	)

	if err := row.Scan(&kid, &name, &algorithm, &secret, &created, &revoked); err != nil {
		return nil, err
	}

	parsed, err := zone.ParseName(name)
	if err != nil {
		return nil, corrupt("tsig_keys", kid, "name", err)
	}

	k.ID = store.TSIGKeyID(kid)
	k.Name = parsed
	k.Algorithm = zone.TSIGAlgorithm(algorithm)
	k.Secret = secret
	k.CreatedAt = fromMillis(created)
	k.RevokedAt = fromNullMillis(revoked)

	return &k, nil
}
