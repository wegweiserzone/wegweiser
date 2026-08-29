package apply

import (
	"context"
	"fmt"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// tokenLock is the key the token operations serialize on. Tokens belong to no
// zone, and the identifier space they share this lock with cannot collide with
// it: a zone identifier is 26 characters of Crockford base32.
const tokenLock = "tokens"

// TokenOpKind is what a batch does to an API token.
type TokenOpKind string

const (
	// TokenCreate brings a token into being.
	TokenCreate TokenOpKind = "create"
	// TokenRevoke ends one. The row stays, so the history keeps naming the
	// token behind each change (D5).
	TokenRevoke TokenOpKind = "revoke"
)

// TokenOp is one change to the tokens this server accepts.
//
// What travels is the hash, never the secret: D5 stores a token as the SHA-256
// of what the client holds, so there is nothing here that authenticates
// anybody even if it is read.
type TokenOp struct {
	Kind TokenOpKind

	// Token is the whole token a create writes.
	Token *store.Token

	// TokenID and At are the revocation.
	TokenID store.TokenID
	At      time.Time
}

// TokenGuard is asked, inside the planning transaction, whether a revocation
// may go ahead. What makes a token an administrator's is the API's vocabulary
// rather than the store's, so the rule lives with the caller and the atomicity
// lives here.
type TokenGuard func(tokens []*store.Token) error

// CreateToken writes a token.
//
// The token is minted by the caller, on the node that accepted the request,
// because the secret is shown to whoever asked and never again. Everything
// stored about it travels.
func (a *Applier) CreateToken(ctx context.Context, tok *store.Token) error {
	b, err := a.PlanCreateToken(tok)
	if err != nil {
		return err
	}
	return a.ApplyBatch(ctx, b)
}

// PlanCreateToken works out the batch that writes a token.
func (a *Applier) PlanCreateToken(tok *store.Token) (*Batch, error) {
	if tok == nil {
		return nil, fmt.Errorf("%w: no token given", zone.ErrInvalid)
	}
	if tok.CreatedAt.IsZero() {
		tok.CreatedAt = a.now().UTC()
	}
	return &Batch{Tokens: []TokenOp{{Kind: TokenCreate, Token: tok}}}, nil
}

// RevokeToken withdraws a token, if guard allows it.
//
// The read the guard judges and the write it permits are one transaction and
// one lock, so two revocations racing cannot each see the other's token and
// both go ahead.
func (a *Applier) RevokeToken(
	ctx context.Context, tid store.TokenID, guard TokenGuard,
) error {
	unlock := a.locks.lock(tokenLock)
	defer unlock()

	b, err := a.PlanRevokeToken(ctx, tid, guard)
	if err != nil {
		return err
	}
	return a.ApplyBatch(ctx, b)
}

// PlanRevokeToken works out the batch that ends a token, and refuses here
// rather than while writing: a follower must not be in a position to disagree
// (D24).
func (a *Applier) PlanRevokeToken(
	ctx context.Context, tid store.TokenID, guard TokenGuard,
) (*Batch, error) {
	if tid == "" {
		return nil, fmt.Errorf("%w: no token named to revoke", zone.ErrInvalid)
	}

	at := a.now().UTC()
	var b *Batch
	err := a.store.View(ctx, func(r store.Reader) error {
		if guard != nil {
			toks, lerr := r.ListTokens(ctx)
			if lerr != nil {
				return lerr
			}
			if gerr := guard(toks); gerr != nil {
				return gerr
			}
		}
		b = &Batch{Tokens: []TokenOp{{Kind: TokenRevoke, TokenID: tid, At: at}}}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return b, nil
}

// writeTokens carries out the token changes a batch holds.
func (b *Batch) writeTokens(ctx context.Context, tx store.Tx) error {
	for _, op := range b.Tokens {
		switch op.Kind {
		case TokenCreate:
			if op.Token == nil {
				return fmt.Errorf("%w: a token creation carries no token", zone.ErrInvalid)
			}
			if err := tx.CreateToken(ctx, op.Token); err != nil {
				return err
			}
		case TokenRevoke:
			if err := tx.RevokeToken(ctx, op.TokenID, op.At); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w token operation %q", zone.ErrInvalid, op.Kind)
		}
	}
	return nil
}
