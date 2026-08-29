package apply

import (
	"context"
	"fmt"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/id"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// KeyOpKind is what a batch does to a transfer key.
type KeyOpKind string

const (
	// KeyCreate brings a key into being.
	KeyCreate KeyOpKind = "create"
	// KeyRevoke ends one, and takes its secret with it.
	KeyRevoke KeyOpKind = "revoke"
)

// KeyOp is one change to the keys a secondary signs with.
//
// A key is not zone data and produces no commit, so a batch can carry one of
// these and nothing else (D32).
type KeyOp struct {
	Kind KeyOpKind

	// Key is the whole key a create writes, secret included. It has to travel:
	// verifying a signature means recomputing the MAC, so a node without the
	// material cannot answer a signed request at all (D28). What keeps that
	// off the wire in the open is the transport, which D24 authenticates.
	Key *store.TSIGKey

	// KeyID and At are the revocation.
	KeyID store.TSIGKeyID
	At    time.Time
}

// CreateKey brings a transfer key into being and returns it.
//
// The secret is the caller's: generated or pasted in on the node that accepted
// the request, which is where D24 wants it settled. What this adds is the
// identifier and the moment, so that two nodes cannot hold one key under two
// identities or two ages.
func (a *Applier) CreateKey(
	ctx context.Context, name zone.Name, alg zone.TSIGAlgorithm, secret []byte,
) (*store.TSIGKey, error) {
	b, key, err := a.PlanCreateKey(name, alg, secret)
	if err != nil {
		return nil, err
	}
	if aerr := a.ApplyBatch(ctx, b); aerr != nil {
		return nil, aerr
	}
	return key, nil
}

// PlanCreateKey works out the batch that writes a key, and mints what the key
// needs beyond what the caller gave.
func (a *Applier) PlanCreateKey(
	name zone.Name, alg zone.TSIGAlgorithm, secret []byte,
) (*Batch, *store.TSIGKey, error) {
	if name.IsZero() {
		return nil, nil, fmt.Errorf("%w: a transfer key needs a name both ends agree on",
			zone.ErrInvalid)
	}
	if !alg.Valid() {
		return nil, nil, fmt.Errorf("%w: %q is not an algorithm this server signs with",
			zone.ErrInvalid, alg)
	}
	if len(secret) == 0 {
		return nil, nil, fmt.Errorf("%w: a transfer key with no secret signs nothing",
			zone.ErrInvalid)
	}

	key := &store.TSIGKey{
		ID:        store.TSIGKeyID(id.New()),
		Name:      name,
		Algorithm: alg,
		Secret:    secret,
		CreatedAt: a.now().UTC(),
	}
	return &Batch{Keys: []KeyOp{{Kind: KeyCreate, Key: key}}}, key, nil
}

// RevokeKey ends a key and clears its secret.
func (a *Applier) RevokeKey(ctx context.Context, kid store.TSIGKeyID) error {
	b, err := a.PlanRevokeKey(kid)
	if err != nil {
		return err
	}
	return a.ApplyBatch(ctx, b)
}

// PlanRevokeKey works out the batch that ends a key. The moment is settled
// here rather than while writing, so every node records the same one.
func (a *Applier) PlanRevokeKey(kid store.TSIGKeyID) (*Batch, error) {
	if kid == "" {
		return nil, fmt.Errorf("%w: no key named to revoke", zone.ErrInvalid)
	}
	return &Batch{Keys: []KeyOp{{Kind: KeyRevoke, KeyID: kid, At: a.now().UTC()}}}, nil
}

// writeKeys carries out the key changes a batch holds.
func (b *Batch) writeKeys(ctx context.Context, tx store.Tx) error {
	for _, op := range b.Keys {
		switch op.Kind {
		case KeyCreate:
			if op.Key == nil {
				return fmt.Errorf("%w: a key creation carries no key", zone.ErrInvalid)
			}
			if err := tx.CreateTSIGKey(ctx, op.Key); err != nil {
				return err
			}
		case KeyRevoke:
			if err := tx.RevokeTSIGKey(ctx, op.KeyID, op.At); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w key operation %q", zone.ErrInvalid, op.Kind)
		}
	}
	return nil
}
